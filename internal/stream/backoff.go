package stream

import (
	"math"
	"time"
)

// Backoff computes an exponential delay with downward-only jitter.
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Factor float64
	Jitter float64
}

func (b Backoff) Next(attempt int, rnd func() float64) time.Duration {
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	maximum := b.Max
	if maximum <= 0 {
		maximum = 60 * time.Second
	}
	factor := b.Factor
	if math.IsNaN(factor) {
		factor = 2
	}
	if factor <= 0 {
		factor = 2
	}
	jitter := b.Jitter
	if math.IsNaN(jitter) {
		jitter = 0
	}
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	if attempt < 0 {
		attempt = 0
	}
	d := float64(base)
	for i := 0; i < attempt && d < float64(maximum); i++ {
		d *= factor
		if math.IsInf(d, 1) || d > float64(maximum) {
			d = float64(maximum)
		}
	}
	if d > float64(maximum) {
		d = float64(maximum)
	}
	random := 0.0
	if rnd != nil {
		random = rnd()
	}
	if math.IsNaN(random) {
		random = 0
	}
	if random < 0 {
		random = 0
	}
	if random > 1 {
		random = 1
	}
	return time.Duration(d * (1 - jitter*random))
}
