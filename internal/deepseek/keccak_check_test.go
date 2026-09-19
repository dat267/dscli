package deepseek

import (
	"fmt"
	"testing"
)

func TestKeccakKnownVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"},
		{"abc", "4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45"},
	}
	for _, c := range cases {
		got := fmt.Sprintf("%x", keccak256v24([]byte(c.in)))
		if got != c.want {
			t.Errorf("keccak256v24(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
