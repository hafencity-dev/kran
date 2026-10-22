package cli

import (
	"regexp"
	"strings"
)

var (
	hostEnvExact = map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"PWD": true, "OLDPWD": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"DISPLAY": true, "GOROOT": true, "GOPATH": true, "GOCACHE": true,
		"GOMODCACHE": true, "GOTOOLCHAIN": true, "PNPM_HOME": true, "npm_config_cache": true,
	}
	hostEnvPrefix   = []string{"XDG_", "SSH_", "WAYLAND_", "DBUS_", "TERM_PROGRAM", "NVM_", "LD_", "DYLD_"}
	hostEnvSuffix   = []string{"_SOCK", "_SOCKET"}
	secretEnvPrefix = []string{"AWS_", "GH_", "GITHUB_", "ANTHROPIC_", "OPENAI_"}
	secretEnvRe     = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|PRIVATE|CREDENTIAL|API_KEY|APIKEY|ACCESS_KEY|(^|_)AUTH(_|$))`)
	// urlCredRe finds a password in a URL value (postgres://u:pw@host).
	urlCredRe = regexp.MustCompile(`://[^/@\s]*:[^/@\s]*@`)
	// alwaysEnv is forwarded no matter what else matches.
	alwaysEnvExact  = map[string]bool{"TERM": true, "COLORTERM": true, "NO_COLOR": true, "FORCE_COLOR": true, "CI": true, "LANG": true, "TZ": true, "KRAN_OWNER": true}
	alwaysEnvPrefix = []string{"LC_"}
	// neverForward stays host-specific even when the user lists it: forwarding
	// these would break the target.
	neverForward = map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "PWD": true, "OLDPWD": true, "TMPDIR": true, "SHELL": true}
)

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, p := range suffixes {
		if strings.HasSuffix(s, p) {
			return true
		}
	}
	return false
}

func isHostEnv(name string) bool {
	if hostEnvExact[name] || hasAnyPrefix(name, hostEnvPrefix) || hasAnySuffix(name, hostEnvSuffix) {
		return true
	}
	return strings.HasPrefix(name, "KRAN_") && name != "KRAN_OWNER"
}

func isSecretEnv(name string) bool {
	return hasAnyPrefix(name, secretEnvPrefix) || secretEnvRe.MatchString(name)
}

// FilterEnv applies SPEC §6.5 to environ ("K=V" entries). explicit lists the
// names the user asked to forward (.kran.toml env.forward and --env); they
// override the secret and host-specific filters, except for neverForward.
func FilterEnv(environ []string, explicit []string) map[string]string {
	want := make(map[string]bool, len(explicit))
	for _, n := range explicit {
		want[n] = true
	}
	out := make(map[string]string)
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		name, val := kv[:i], kv[i+1:]
		switch {
		case alwaysEnvExact[name] || hasAnyPrefix(name, alwaysEnvPrefix) && !isSecretEnv(name):
			out[name] = val
		case want[name] && !neverForward[name] && !strings.HasPrefix(name, "KRAN_"):
			out[name] = val
		case isHostEnv(name), isSecretEnv(name), urlCredRe.MatchString(val):
			// dropped
		default:
			out[name] = val
		}
	}
	return out
}
