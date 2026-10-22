//go:build !e2e && !chaos && !smoke

package e2e

// beforeAll: the default build runs only the in-process tests.
func beforeAll() func() { return func() {} }
