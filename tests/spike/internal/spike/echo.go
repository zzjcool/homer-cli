package spike

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const EchoSubprotocol = "homer.stream.v1"

// EchoConfig controls the local WebSocket and NDJSON test origin.
type EchoConfig struct {
	Addr           string
	WriteTimeout   time.Duration
	NoHijacker     bool
	PushInterval   time.Duration
	PushCount      int
	CloseCode      int
	CloseReason    string
	NDJSONCount    int
	NDJSONInterval time.Duration
	MaxReadSize    int64
}

type echoServer struct {
	cfg EchoConfig
}

// ServeEcho runs the isolated test origin until ctx is canceled or the HTTP
// server fails. The server intentionally has no authentication because it is
// only intended to bind to loopback during the spike.
func ServeEcho(ctx context.Context, cfg EchoConfig) error {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:17801"
	}
	if cfg.NDJSONCount <= 0 {
		cfg.NDJSONCount = 5
	}
	if cfg.NDJSONInterval <= 0 {
		cfg.NDJSONInterval = time.Second
	}
	if cfg.MaxReadSize <= 0 {
		cfg.MaxReadSize = 16 << 20
	}

	e := &echoServer{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ws", e.handleWS)
	mux.HandleFunc("/no-hijacker/ws", func(w http.ResponseWriter, r *http.Request) {
		e.handleWS(noHijackerResponseWriter{ResponseWriter: w}, r)
	})
	mux.HandleFunc("/ndjson", e.handleNDJSON)

	var handler http.Handler = mux
	if cfg.NoHijacker {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mux.ServeHTTP(noHijackerResponseWriter{ResponseWriter: w}, r)
		})
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.WriteTimeout,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("wsecho listening addr=%s write_timeout=%s no_hijacker=%t", cfg.Addr, cfg.WriteTimeout, cfg.NoHijacker)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shut down echo server: %w", err)
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// noHijackerResponseWriter deliberately hides optional ResponseWriter
// interfaces, including http.Hijacker, as a middleware that fails to unwrap
// its writer would.
type noHijackerResponseWriter struct {
	http.ResponseWriter
}

func (e *echoServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{EchoSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// Do not include request headers (which may contain credentials) in logs.
		log.Printf("websocket accept failed path=%s: %v", r.URL.Path, err)
		return
	}
	conn.SetReadLimit(e.cfg.MaxReadSize)
	defer conn.CloseNow()

	requestHeaders := make(http.Header, len(r.Header))
	for key, values := range r.Header {
		requestHeaders[key] = append([]string(nil), values...)
	}
	meta := struct {
		Type           string      `json:"type"`
		RequestHeaders http.Header `json:"requestHeaders"`
		Subprotocol    string      `json:"subprotocol"`
	}{
		Type:           "upgrade",
		RequestHeaders: requestHeaders,
		Subprotocol:    conn.Subprotocol(),
	}
	if err := writeJSON(conn, r.Context(), meta); err != nil {
		return
	}

	if code, reason, ok := e.closeOptions(r); ok {
		_ = conn.Close(websocket.StatusCode(code), reason)
		return
	}

	var writeMu sync.Mutex
	writeMessage := func(ctx context.Context, typ websocket.MessageType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.Write(ctx, typ, payload)
	}

	if e.cfg.PushInterval > 0 && e.cfg.PushCount > 0 {
		go func() {
			ticker := time.NewTicker(e.cfg.PushInterval)
			defer ticker.Stop()
			for seq := 1; seq <= e.cfg.PushCount; seq++ {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					payload, _ := json.Marshal(map[string]any{
						"type": "push",
						"seq":  seq,
						"at":   time.Now().UTC().Format(time.RFC3339Nano),
					})
					writeCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					err := writeMessage(writeCtx, websocket.MessageText, payload)
					cancel()
					if err != nil {
						return
					}
				}
			}
		}()
	}

	for {
		typ, payload, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var request struct {
				Type string          `json:"type"`
				ID   string          `json:"id"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal(payload, &request) == nil {
				switch request.Type {
				case "app-ping":
					response, _ := json.Marshal(map[string]string{"type": "app-pong", "id": request.ID})
					if err := writeMessage(r.Context(), websocket.MessageText, response); err != nil {
						return
					}
					continue
				case "request-push":
					response, _ := json.Marshal(map[string]any{"type": "push", "id": request.ID, "data": request.Data})
					if err := writeMessage(r.Context(), websocket.MessageText, response); err != nil {
						return
					}
					continue
				}
			}
		}
		if err := writeMessage(r.Context(), typ, payload); err != nil {
			return
		}
	}
}

func (e *echoServer) closeOptions(r *http.Request) (int, string, bool) {
	code := e.cfg.CloseCode
	reason := e.cfg.CloseReason
	if queryCode := r.URL.Query().Get("closeCode"); queryCode != "" {
		if parsed, err := strconv.Atoi(queryCode); err == nil {
			code = parsed
		}
	}
	if queryReason := r.URL.Query().Get("closeReason"); queryReason != "" {
		reason = queryReason
	}
	if code == 0 {
		return 0, "", false
	}
	// The close control frame has a 125-byte total payload limit; the status
	// code consumes two bytes, so keep the reason within 123 UTF-8 bytes.
	for len(reason) > 123 {
		reason = reason[:len(reason)-1]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	return code, reason, true
}

func (e *echoServer) handleNDJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "response writer does not implement http.Flusher", http.StatusInternalServerError)
		return
	}
	for seq := 1; seq <= e.cfg.NDJSONCount; seq++ {
		line, err := json.Marshal(map[string]any{
			"seq": seq,
			"at":  time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			http.Error(w, "encode ndjson line", http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			return
		}
		flusher.Flush()
		if seq != e.cfg.NDJSONCount {
			timer := time.NewTimer(e.cfg.NDJSONInterval)
			select {
			case <-r.Context().Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
		}
	}
}

func writeJSON(conn *websocket.Conn, ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

// URLForPath preserves a base URL's scheme/authority and joins a path for
// callers that need to address the local HTTP test endpoints.
func URLForPath(base, path string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}
