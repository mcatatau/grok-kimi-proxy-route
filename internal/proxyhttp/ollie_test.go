package proxyhttp

import "testing"

func TestSanitizeOllieChatBodyNoopOnNil(t *testing.T) {
	// ensure package builds; real sanitizer lives in server.go
	sanitizeOllieChatBody(nil)
}
