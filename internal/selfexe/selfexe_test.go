package selfexe

import (
	"errors"
	"testing"
)

func TestPathRefusesATestBinaryUntilAllowed(t *testing.T) {
	if _, err := Path(); !errors.Is(err, ErrTestBinary) {
		t.Fatalf("Path in a test binary = %v, want ErrTestBinary", err)
	}
	AllowTest()
	defer allowTest.Store(false)
	if p, err := Path(); err != nil || p == "" {
		t.Fatalf("Path after AllowTest = %q, %v", p, err)
	}
}
