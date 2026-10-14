package overlay

import "math"

// spring is a damped harmonic follower for smooth size morphs.
type spring struct {
	x, v, target float64
	init         bool
}

const (
	springK    = 420.0 // stiffness
	springZeta = 0.82  // damping ratio: a hint of overshoot, no wobble
)

// set moves the target; snap jumps there immediately.
func (s *spring) set(target float64, snap bool) {
	s.target = target
	if snap || !s.init {
		s.x, s.v, s.init = target, 0, true
	}
}

// step advances by dt seconds and reports whether it is still moving.
func (s *spring) step(dt float64) bool {
	c := 2 * springZeta * math.Sqrt(springK)
	for dt > 0 {
		h := math.Min(dt, 1.0/240)
		a := springK*(s.target-s.x) - c*s.v
		s.v += a * h
		s.x += s.v * h
		dt -= h
	}
	if math.Abs(s.target-s.x) < 0.05 && math.Abs(s.v) < 0.5 {
		s.x, s.v = s.target, 0
		return false
	}
	return true
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// easeOut is a cubic ease-out over t in 0..1.
func easeOut(t float64) float64 {
	t = clamp(t, 0, 1)
	return 1 - math.Pow(1-t, 3)
}

// ramp returns progress 0..1 of elapsed over duration.
func ramp(elapsed, duration float64) float64 {
	if duration <= 0 {
		return 1
	}
	return clamp(elapsed/duration, 0, 1)
}
