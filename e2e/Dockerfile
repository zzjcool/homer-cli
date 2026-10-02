# homer-cli e2e image: build stage + runtime with git for gitx.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/homer ./cmd/homer \
	&& CGO_ENABLED=0 go build -o /out/keys-server ./e2e/keys-server

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates curl
# Tests inside containers commit to the local origin; a fixed identity keeps
# git happy without touching the host config.
ENV GIT_CONFIG_GLOBAL=/etc/gitconfig
RUN printf '[user]\n\tname = Homer E2E\n\temail = homer-e2e@example.invalid\n' > /etc/gitconfig \
    && printf '[safe]\n\tdirectory = *\n' >> /etc/gitconfig
COPY --from=build /out/homer /usr/local/bin/homer
COPY --from=build /out/keys-server /usr/local/bin/keys-server
ENTRYPOINT ["homer"]
