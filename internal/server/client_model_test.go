package server

import "testing"

func TestSetModelField(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"id":"x","model":"m","choices":[]}`, `{"id":"x","model":"gpu-b/m","choices":[]}`},
		{`data: {"model" : "m","x":1}` + "\n", `data: {"model" : "gpu-b/m","x":1}` + "\n"},
		{`{"model":"gpu-b/m"}`, `{"model":"gpu-b/m"}`},                                                                             // already right
		{`{"choices":[{"delta":{"content":"say \"model\":\"x\""}}]}`, `{"choices":[{"delta":{"content":"say \"model\":\"x\""}}]}`}, // escaped: not a key
		{`{"served_model":"m"}`, `{"served_model":"m"}`},
		{`{"model":"a\"b","n":1}`, `{"model":"gpu-b/m","n":1}`},
		{`{"model":"trunc`, `{"model":"trunc`},
	} {
		if got := string(setModelField([]byte(c.in), "gpu-b/m")); got != c.want {
			t.Errorf("setModelField(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}
