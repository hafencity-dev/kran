package cli

import (
	"reflect"
	"testing"
)

func TestFilterEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin", "HOME=/home/x", "USER=x", "LOGNAME=x", "SHELL=/bin/zsh", "PWD=/w", "OLDPWD=/o",
		"TMPDIR=/t", "TMP=/t", "TEMP=/t", "XDG_RUNTIME_DIR=/run", "XDG_CONFIG_HOME=/c", "SSH_AUTH_SOCK=/s",
		"SSH_CONNECTION=1", "DISPLAY=:0", "WAYLAND_DISPLAY=w", "DBUS_SESSION_BUS_ADDRESS=d",
		"TERM_PROGRAM=iTerm", "TERM_PROGRAM_VERSION=3", "GOROOT=/g", "GOPATH=/gp", "GOCACHE=/gc",
		"GOMODCACHE=/gm", "GOTOOLCHAIN=local", "NVM_DIR=/n", "NVM_BIN=/nb", "PNPM_HOME=/p", "npm_config_cache=/nc",
		"GPG_AGENT_SOCK=/a", "DOCKER_SOCKET=/d", "LD_LIBRARY_PATH=/l", "DYLD_LIBRARY_PATH=/l",
		"KRAN_STATE_DIR=/s", "KRAN_OWNER=claude:x", "KRAN_JOB_ID=1",
		"GITHUB_TOKEN=a", "GH_HOST=h", "AWS_REGION=eu", "ANTHROPIC_MODEL=m", "OPENAI_BASE=b",
		"MY_SECRET=1", "DB_PASSWORD=1", "stripe_api_key=1", "SOME_PRIVATE_THING=1", "X_CREDENTIALS=1", "S3_ACCESS_KEY=1", "PASSWD_FILE=1",
		"DATABASE_URL=postgres://x", "NODE_ENV=test", "TERM=xterm-256color", "COLORTERM=truecolor", "NO_COLOR=1",
		"FORCE_COLOR=1", "CI=1", "LANG=C.UTF-8", "LC_ALL=C", "LC_CTYPE=C", "TZ=UTC", "FOO=bar=baz", "EMPTY=",
	}
	got := FilterEnv(env, nil)
	want := map[string]string{
		"KRAN_OWNER": "claude:x", "DATABASE_URL": "postgres://x", "NODE_ENV": "test", "TERM": "xterm-256color",
		"COLORTERM": "truecolor", "NO_COLOR": "1", "FORCE_COLOR": "1", "CI": "1", "LANG": "C.UTF-8",
		"LC_ALL": "C", "LC_CTYPE": "C", "TZ": "UTC", "FOO": "bar=baz", "EMPTY": "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestFilterEnvExplicit(t *testing.T) {
	env := []string{"GITHUB_TOKEN=a", "MY_SECRET=b", "PATH=/x", "HOME=/h", "XDG_FOO=1", "KRAN_JOB_ID=2", "OTHER_TOKEN=c"}
	got := FilterEnv(env, []string{"GITHUB_TOKEN", "MY_SECRET", "PATH", "HOME", "XDG_FOO", "KRAN_JOB_ID"})
	want := map[string]string{"GITHUB_TOKEN": "a", "MY_SECRET": "b", "XDG_FOO": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestFilterEnvIgnoresMalformed(t *testing.T) {
	got := FilterEnv([]string{"NOEQUALS", "=x", "A=1"}, nil)
	if !reflect.DeepEqual(got, map[string]string{"A": "1"}) {
		t.Fatal(got)
	}
}

// Credentials the SPEC §6.5 name list misses: auth blobs, passphrases,
// passwords inside URLs, and LC_* names (used to smuggle values through
// ssh AcceptEnv) that look like secrets.
func TestFilterEnvMoreSecrets(t *testing.T) {
	env := []string{
		"npm_config__auth=dXNlcjpwYXNz", "COMPOSER_AUTH={}", "DOCKER_AUTH_CONFIG={}", "GPG_PASSPHRASE=x", "BORG_PASSPHRASE=x",
		"DATABASE_URL=postgres://u:pw@db/x", "PIP_INDEX_URL=https://u:tok@pypi.example/simple", "REDIS_URL=redis://:pw@r:6379",
		"LC_API_TOKEN=x",
		// kept
		"GIT_AUTHOR_NAME=me", "AUTHOR=me", "PLAIN_URL=postgres://db/x", "PROXY=http://proxy:3128", "LC_ALL=C",
		"MAIL=me@example.com",
	}
	got := FilterEnv(env, nil)
	want := map[string]string{"GIT_AUTHOR_NAME": "me", "AUTHOR": "me", "PLAIN_URL": "postgres://db/x", "PROXY": "http://proxy:3128", "LC_ALL": "C", "MAIL": "me@example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if got := FilterEnv([]string{"DATABASE_URL=postgres://u:pw@db/x"}, []string{"DATABASE_URL"}); got["DATABASE_URL"] == "" {
		t.Fatal("an explicitly forwarded URL with a password must pass")
	}
}
