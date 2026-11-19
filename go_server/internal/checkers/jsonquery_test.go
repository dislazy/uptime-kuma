package checkers

import (
	"strings"
	"testing"
)

func TestJSONataExpressions(t *testing.T) {
	body := []byte(`{"values":[1,2,3],"items":[{"name":"a","ok":true},{"name":"b","ok":false}],"nested":{"deep":{"v":"42"}}}`)

	cases := []struct {
		expr string
		want string
	}{
		{"$sum(values)", "6"},
		{"$count(items)", "2"},
		{"items[ok=true].name[0]", "a"},
		{"nested.deep.v", "42"},
		{"$string($sum(values) * 2)", "12"},
	}

	for _, c := range cases {
		got, err := evalJSONPath(body, c.expr)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", c.expr, err)
		}
		if s := stringify(got); s != c.want {
			t.Fatalf("%s: want %q, got %q", c.expr, c.want, s)
		}
	}
}

func TestJSONataUndefinedAndArray(t *testing.T) {
	if _, err := evalJSONPath([]byte(`{"a":1}`), "missing.path"); err == nil ||
		!strings.Contains(err.Error(), "Empty or undefined response") {
		t.Fatalf("want undefined error, got %v", err)
	}

	if _, err := evalJSONPath([]byte(`{"a":[1,2]}`), "a"); err == nil ||
		!strings.Contains(err.Error(), "returned the array") {
		t.Fatalf("want array error, got %v", err)
	}
}
