package parser

import (
	"unicode"

	"github.com/antlr4-go/antlr/v4"
)

// CaseChangingStream wraps an existing CharStream, but upper cases, or
// lower cases the input before it is tokenized.
type CaseChangingStream struct {
	antlr.CharStream
	upper bool
}

// newCaseChangingStream returns a new CaseChangingStream that forces
// all tokens read from the underlying stream to be either upper case
// or lower case based on the upper argument.
func newCaseChangingStream(in antlr.CharStream, upper bool) antlr.CharStream {
	return &CaseChangingStream{in, upper}
}

// LA gets the value of the symbol at offset from the current position
// from the underlying CharStream and converts it to either upper case
// or lower case.
func (is *CaseChangingStream) LA(offset int) int {
	in := is.CharStream.LA(offset)

	if in < 0 {
		// Such as antlr.TokenEOF which is -1
		return in
	}

	if is.upper {
		return int(unicode.ToUpper(rune(in)))
	}

	return int(unicode.ToLower(rune(in)))
}
