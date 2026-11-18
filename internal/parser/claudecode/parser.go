package claudecode

import (
	"sync/atomic"

	"github.com/harnext/harnext-agent/internal/analyzer"
)

// Parser parses transcript lines, optionally analyzing content with an Extractor. The
// Extractor can be swapped at run time (e.g. when a new signed configuration arrives).
// A nil *Parser and a Parser without an Extractor behave exactly like ParseLine: no
// content is analyzed and events carry Analyzed=false.
type Parser struct {
	ex atomic.Pointer[extHolder]
}

type extHolder struct{ ex analyzer.Extractor }

// NewParser returns a Parser; ex may be nil.
func NewParser(ex analyzer.Extractor) *Parser {
	p := &Parser{}
	p.SetExtractor(ex)
	return p
}

// SetExtractor replaces the Extractor; nil switches content analysis off.
func (p *Parser) SetExtractor(ex analyzer.Extractor) {
	if ex == nil {
		p.ex.Store(nil)
		return
	}
	p.ex.Store(&extHolder{ex: ex})
}

// ParseLine interprets one transcript line; see the package-level ParseLine.
func (p *Parser) ParseLine(line []byte) Event {
	if p == nil {
		return parseLine(line, nil)
	}
	var ex analyzer.Extractor
	if h := p.ex.Load(); h != nil {
		ex = h.ex
	}
	return parseLine(line, ex)
}
