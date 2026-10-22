package metrics

import (
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestStressed(t *testing.T) {
	now := time.Now()
	linux := proto.NodeInfo{OS: "linux", CPUs: 20}
	mac := proto.NodeInfo{OS: "darwin", CPUs: 16}
	cases := []struct {
		name string
		info proto.NodeInfo
		m    proto.NodeMetrics
		want bool
	}{
		{"no sample yet", linux, proto.NodeMetrics{Load1: 100}, false},
		{"calm", linux, proto.NodeMetrics{Time: now, Load1: 4, MemPressure: 0.5}, false},
		{"busy but fine", linux, proto.NodeMetrics{Time: now, Load1: 22, CPUPressure: 30}, false},
		{"load 40 on 20 cpus", linux, proto.NodeMetrics{Time: now, Load1: 40}, true},
		{"memory psi 40", linux, proto.NodeMetrics{Time: now, Load1: 5, MemPressure: 40}, true},
		{"io psi", linux, proto.NodeMetrics{Time: now, IOPressure: 60}, true},
		{"cpu psi", linux, proto.NodeMetrics{Time: now, CPUPressure: 85}, true},
		{"swap full and no memory", linux, proto.NodeMetrics{Time: now, SwapTotal: 8 << 30, SwapUsed: 8 << 30, MemAvailable: 100 << 20}, true},
		// macOS maps memorystatus to 0..100 where 60 is normal use.
		{"mac normal", mac, proto.NodeMetrics{Time: now, Load1: 6, MemPressure: 65}, false},
		{"mac critical", mac, proto.NodeMetrics{Time: now, Load1: 6, MemPressure: 95}, true},
	}
	for _, c := range cases {
		if got := Stressed(c.m, c.info); got != c.want {
			t.Errorf("%s: Stressed = %v, want %v", c.name, got, c.want)
		}
	}
}
