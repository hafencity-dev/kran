package daemon

import (
	"os"
	"os/user"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

// originEnv describes this node's user, time zone and native toolchains
// for the jobs it submits: a container job elsewhere uses them to look
// like this machine (SPEC §6.8).
func originEnv(rts []proto.Runtime) *proto.OriginEnv {
	oe := &proto.OriginEnv{TZ: localTZ()}
	if u, err := user.Current(); err == nil {
		oe.User, oe.Home = u.Username, u.HomeDir
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		oe.Home = h
	}
	for _, rt := range rts {
		if rt.Kind == proto.RuntimeNative && len(rt.Toolchains) > 0 {
			oe.Toolchains = make(map[string]string, len(rt.Toolchains))
			for k, v := range rt.Toolchains {
				oe.Toolchains[k] = v
			}
		}
	}
	return oe
}

// localTZ returns the IANA name of the local time zone: $TZ when it names
// one, else the target of /etc/localtime (…/zoneinfo/Europe/Berlin on
// Linux and macOS); "" when unknown.
func localTZ() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if i := strings.Index(tz, "zoneinfo/"); i >= 0 {
			tz = tz[i+len("zoneinfo/"):]
		}
		if !strings.HasPrefix(tz, "/") {
			return tz
		}
		return ""
	}
	return zoneFromLink(os.Readlink("/etc/localtime"))
}

func zoneFromLink(target string, err error) string {
	if err != nil {
		return ""
	}
	if i := strings.LastIndex(target, "zoneinfo/"); i >= 0 {
		return target[i+len("zoneinfo/"):]
	}
	return ""
}
