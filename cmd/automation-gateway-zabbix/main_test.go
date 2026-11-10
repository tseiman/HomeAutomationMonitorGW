package main

import "testing"

func TestCommandAcceptsOnlyConfigFlag(t *testing.T) {
	if err := run([]string{}); err == nil {
		t.Fatal("missing config accepted")
	}
	if err := run([]string{"-gateway-token", "secret"}); err == nil {
		t.Fatal("secret CLI flag accepted")
	}
}
