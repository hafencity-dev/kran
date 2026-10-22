package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseSize parses "8GiB", "512M", "6g", "1.5GiB" or a plain byte count.
// K/M/G/T and KiB/MiB/GiB/TiB are all binary (1024), matching how people
// write memory sizes for tools.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size; write something like 8GiB")
	}
	i := len(t)
	for i > 0 && (t[i-1] < '0' || t[i-1] > '9') && t[i-1] != '.' {
		i--
	}
	num, unit := strings.TrimSpace(t[:i]), strings.ToLower(strings.TrimSpace(t[i:]))
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid size %q; write something like 8GiB or 512MiB", s)
	}
	mult := map[string]float64{
		"": 1, "b": 1,
		"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
		"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
		"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
		"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
	}
	m, ok := mult[unit]
	if !ok {
		return 0, fmt.Errorf("invalid size unit %q in %q; use KiB, MiB, GiB or TiB", unit, s)
	}
	return int64(f * m), nil
}

// FormatSize renders bytes as the shortest exact-ish binary size.
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	if v == math.Trunc(v) {
		return fmt.Sprintf("%d%s", int64(v), suffix)
	}
	return fmt.Sprintf("%.1f%s", v, suffix)
}
