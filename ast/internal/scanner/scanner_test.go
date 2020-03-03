package scanner

import (
	"bytes"
	"testing"

	"github.com/open-policy-agent/opa/ast/internal/tokens"
)

func TestPositions(t *testing.T) {
	tests := []struct {
		note       string
		input      string
		wantOffset int
		wantEnd    int
	}{
		{
			note:       "symbol",
			input:      "(",
			wantOffset: 0,
			wantEnd:    1,
		},
		{
			note:       "ident",
			input:      "foo",
			wantOffset: 0,
			wantEnd:    3,
		},
		{
			note:       "number",
			input:      "100",
			wantOffset: 0,
			wantEnd:    3,
		},
		{
			note:       "string",
			input:      `"foo"`,
			wantOffset: 0,
			wantEnd:    5,
		},
		{
			note:       "string - wide char",
			input:      `"foo÷"`,
			wantOffset: 0,
			wantEnd:    7,
		},
		{
			note:       "comment",
			input:      `# foo`,
			wantOffset: 0,
			wantEnd:    5,
		},
		{
			note:       "newline",
			input:      "foo\n",
			wantOffset: 0,
			wantEnd:    3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.note, func(t *testing.T) {
			s, err := New(bytes.NewBufferString(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			_, pos, _ := s.Scan()
			if pos.Offset != tc.wantOffset {
				t.Fatalf("want offset %d but got %d", tc.wantOffset, pos.Offset)
			}
			if pos.End != tc.wantEnd {
				t.Fatalf("want end %d but got %d", tc.wantEnd, pos.End)
			}
		})
	}
}

func TestLiterals(t *testing.T) {

	tests := []struct {
		note       string
		input      string
		wantRow    int
		wantOffset int
		wantTok    tokens.Token
		wantLit    string
	}{
		{
			note:       "ascii chars",
			input:      `"hello world"`,
			wantRow:    1,
			wantOffset: 0,
			wantTok:    tokens.String,
			wantLit:    `"hello world"`,
		},
		{
			note:       "wide chars",
			input:      `"¡¡¡foo, bar!!!"`,
			wantRow:    1,
			wantOffset: 0,
			wantTok:    tokens.String,
			wantLit:    `"¡¡¡foo, bar!!!"`,
		},
		{
			note:       "raw strings",
			input:      "`foo`",
			wantRow:    1,
			wantOffset: 0,
			wantTok:    tokens.String,
			wantLit:    "`foo`",
		},
		{
			note:       "raw strings - wide chars",
			input:      "`¡¡¡foo, bar!!!`",
			wantRow:    1,
			wantOffset: 0,
			wantTok:    tokens.String,
			wantLit:    "`¡¡¡foo, bar!!!`",
		},
		{
			note:       "comments",
			input:      "# foo",
			wantRow:    1,
			wantOffset: 0,
			wantTok:    tokens.Comment,
			wantLit:    "# foo",
		},
		{
			note:       "comments - wide chars",
			input:      "#¡foo",
			wantRow:    1,
			wantOffset: 0,
			wantTok:    tokens.Comment,
			wantLit:    "#¡foo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.note, func(t *testing.T) {
			s, err := New(bytes.NewBufferString(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			tok, pos, lit := s.Scan()
			if pos.Row != tc.wantRow {
				t.Errorf("Expected row %d but got %d", tc.wantRow, pos.Row)
			}
			if pos.Offset != tc.wantOffset {
				t.Errorf("Expected offset %d but got %d", tc.wantOffset, pos.Offset)
			}
			if tok != tc.wantTok {
				t.Errorf("Expected token %v but got %v", tc.wantTok, tok)
			}
			if lit != tc.wantLit {
				t.Errorf("Expected literal %v but got %v", tc.wantLit, lit)
			}
		})
	}

}
