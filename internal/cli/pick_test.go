package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestPickNumbered(t *testing.T) {
	var out bytes.Buffer
	i, err := pickNumbered(bufio.NewReader(strings.NewReader("x\n2\n")), &out, "Pick", []string{"a", "b", "c"})
	if err != nil || i != 1 {
		t.Fatal(i, err)
	}
	if !strings.Contains(out.String(), "not one of the numbers") {
		t.Fatal(out.String())
	}
	if _, err := pickNumbered(bufio.NewReader(strings.NewReader("\n")), &out, "Pick", []string{"a"}); err != errPickCanceled {
		t.Fatal(err)
	}
	if _, err := pick(nil, &out, "p", nil); err == nil {
		t.Fatal("empty list must error")
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	for d, want := range map[string]bool{"iOS, online": true, "Windows, offline": true, "macOS, online": false, "linux, online": false, "": false} {
		if unsupportedPlatform(d) != want {
			t.Errorf("%q", d)
		}
	}
}
