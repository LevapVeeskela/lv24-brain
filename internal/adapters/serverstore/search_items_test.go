package serverstore

import (
	"fmt"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestItemSearchFilter(t *testing.T) {
	cases := []struct {
		query     ports.Query
		include   bool
		wantKinds string
	}{
		{ports.Query{}, true, "[]"},
		{ports.Query{ExcludeKinds: []string{"dump"}}, false, "[]"},
		{ports.Query{ExcludeKinds: []string{"item"}}, false, "[]"},
		{ports.Query{ExcludeKinds: []string{"note"}}, true, "[]"},
		{ports.Query{Kinds: []string{"note"}}, false, "[note]"},
		{ports.Query{Kinds: []string{"dump"}}, true, "[dump item]"},
		{ports.Query{Kinds: []string{"item", "note"}}, true, "[item note]"},
	}
	for _, tc := range cases {
		include, kinds := itemSearchFilter(tc.query)
		if include != tc.include || fmt.Sprint(kinds) != tc.wantKinds {
			t.Fatalf("%+v -> %v %v, want %v %s", tc.query, include, kinds, tc.include, tc.wantKinds)
		}
	}
}
