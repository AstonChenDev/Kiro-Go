package proxy

import "strings"

const (
	thinkingSegmentText = iota
	thinkingSegmentStart
	thinkingSegmentDelta
	thinkingSegmentEnd
)

// thinkingStreamParser separates Kiro's two reasoning encodings:
// reasoningContentEvent callbacks and <thinking> blocks embedded in ordinary
// assistantResponseEvent text. It buffers tag-sized tails so split tags never
// leak into client-visible answer text.
type thinkingStreamParser struct {
	enabled bool
	emit    func(text string, state int)

	buffer            string
	inThinkingBlock   bool
	dropTagThinking   bool
	source            thinkingStreamSource
	thinkingStarted   bool
	eventThinkingOpen bool
}

func newThinkingStreamParser(enabled bool, emit func(text string, state int)) *thinkingStreamParser {
	return &thinkingStreamParser{enabled: enabled, emit: emit}
}

func (p *thinkingStreamParser) Push(text string, isThinking bool) {
	p.process(text, isThinking, false)
}

func (p *thinkingStreamParser) Flush() {
	p.process("", false, true)
	if p.eventThinkingOpen {
		p.emitSegment("", thinkingSegmentEnd)
		p.eventThinkingOpen = false
		p.thinkingStarted = false
	}
}

func (p *thinkingStreamParser) Reset() {
	p.buffer = ""
	p.inThinkingBlock = false
	p.dropTagThinking = false
	p.source = thinkingSourceUnknown
	p.thinkingStarted = false
	p.eventThinkingOpen = false
}

func (p *thinkingStreamParser) process(text string, isThinking, forceFlush bool) {
	if isThinking {
		if !p.enabled || !allowReasoningSource(&p.source) {
			return
		}
		if !p.thinkingStarted {
			p.emitSegment(text, thinkingSegmentStart)
			p.thinkingStarted = true
			p.eventThinkingOpen = true
		} else {
			p.emitSegment(text, thinkingSegmentDelta)
		}
		return
	}

	if p.eventThinkingOpen {
		p.emitSegment("", thinkingSegmentEnd)
		p.eventThinkingOpen = false
		p.thinkingStarted = false
	}

	p.buffer += text
	for {
		if !p.inThinkingBlock {
			start := strings.Index(p.buffer, "<thinking>")
			if start >= 0 {
				if start > 0 {
					p.emitSegment(p.buffer[:start], thinkingSegmentText)
				}
				p.buffer = p.buffer[start+len("<thinking>"):]
				p.inThinkingBlock = true
				p.dropTagThinking = !p.enabled || !allowTagSource(&p.source)
				p.thinkingStarted = false
				continue
			}

			if forceFlush || len([]rune(p.buffer)) > 50 {
				runes := []rune(p.buffer)
				safeLen := len(runes)
				if !forceFlush {
					safeLen = max(0, len(runes)-15)
				}
				if safeLen > 0 {
					p.emitSegment(string(runes[:safeLen]), thinkingSegmentText)
					p.buffer = string(runes[safeLen:])
				}
			}
			return
		}

		end := strings.Index(p.buffer, "</thinking>")
		if end >= 0 {
			content := p.buffer[:end]
			p.finishTagThinking(content)
			p.buffer = p.buffer[end+len("</thinking>"):]
			p.inThinkingBlock = false
			p.dropTagThinking = false
			p.thinkingStarted = false
			continue
		}

		if forceFlush {
			p.finishTagThinking(p.buffer)
			p.buffer = ""
			p.inThinkingBlock = false
			p.dropTagThinking = false
			p.thinkingStarted = false
			return
		}

		runes := []rune(p.buffer)
		if len(runes) > 20 {
			safeLen := len(runes) - 15
			if safeLen > 0 {
				p.emitTagThinking(string(runes[:safeLen]))
				p.buffer = string(runes[safeLen:])
			}
		}
		return
	}
}

func (p *thinkingStreamParser) emitTagThinking(text string) {
	if p.dropTagThinking || text == "" {
		return
	}
	if !p.thinkingStarted {
		p.emitSegment(text, thinkingSegmentStart)
		p.thinkingStarted = true
		return
	}
	p.emitSegment(text, thinkingSegmentDelta)
}

func (p *thinkingStreamParser) finishTagThinking(text string) {
	if p.dropTagThinking {
		return
	}
	if !p.thinkingStarted {
		if text == "" {
			return
		}
		p.emitSegment(text, thinkingSegmentStart)
		p.emitSegment("", thinkingSegmentEnd)
		return
	}
	p.emitSegment(text, thinkingSegmentEnd)
}

func (p *thinkingStreamParser) emitSegment(text string, state int) {
	if p.emit != nil {
		p.emit(text, state)
	}
}
