package ast

import (
	"testing"
)

func BenchmarkIndexLookup(b *testing.B) {

	compiler := MustCompileModules(map[string]string{
		"test.rego": `package x

		import rego.v1

		f(x) if {
			x = 1
		}

		f(x) if {
			x = 2
		}
		`,
	})

	index := compiler.RuleIndex(MustParseRef("data.x.f"))
	if index == nil {
		panic("expected to find index")
	}

	args := []Value{
		MustParseTerm(`1`).Value,
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {

		r := testResolver{args: args}

		result, err := index.Lookup(&r)
		if err != nil {
			panic(err)
		}

		if len(result.Rules) != 1 {
			panic("expected one rule")
		}
	}

}
