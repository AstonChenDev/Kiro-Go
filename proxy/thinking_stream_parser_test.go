package proxy

import (
	"strings"
	"testing"
)

type parserCapture struct {
	answer    strings.Builder
	reasoning strings.Builder
	starts    int
	ends      int
}

func (c *parserCapture) emit(text string, state int) {
	switch state {
	case thinkingSegmentText:
		c.answer.WriteString(text)
	case thinkingSegmentStart:
		c.starts++
		c.reasoning.WriteString(text)
	case thinkingSegmentDelta:
		c.reasoning.WriteString(text)
	case thinkingSegmentEnd:
		c.reasoning.WriteString(text)
		c.ends++
	}
}

func TestThinkingStreamParserHandlesSplitTags(t *testing.T) {
	var captured parserCapture
	parser := newThinkingStreamParser(true, captured.emit)
	parser.Push("before<thin", false)
	parser.Push("king>step one</thin", false)
	parser.Push("king>after", false)
	parser.Flush()

	if got := captured.answer.String(); got != "beforeafter" {
		t.Fatalf("answer=%q, want beforeafter", got)
	}
	if got := captured.reasoning.String(); got != "step one" {
		t.Fatalf("reasoning=%q, want step one", got)
	}
	if captured.starts != 1 || captured.ends != 1 {
		t.Fatalf("boundaries starts=%d ends=%d", captured.starts, captured.ends)
	}
}

func TestThinkingStreamParserPrefersNativeReasoningEvents(t *testing.T) {
	var captured parserCapture
	parser := newThinkingStreamParser(true, captured.emit)
	parser.Push("native reason", true)
	parser.Push("<thinking>duplicate reason</thinking>answer", false)
	parser.Flush()

	if got := captured.reasoning.String(); got != "native reason" {
		t.Fatalf("reasoning=%q, duplicate tag source must be dropped", got)
	}
	if got := captured.answer.String(); got != "answer" {
		t.Fatalf("answer=%q, want answer", got)
	}
	if captured.starts != 1 || captured.ends != 1 {
		t.Fatalf("boundaries starts=%d ends=%d", captured.starts, captured.ends)
	}
}

func TestThinkingStreamParserHidesThinkingWhenDisabled(t *testing.T) {
	var captured parserCapture
	parser := newThinkingStreamParser(false, captured.emit)
	parser.Push("<thinking>private reason</thinking>answer", false)
	parser.Push("native private reason", true)
	parser.Flush()

	if captured.reasoning.Len() != 0 || captured.starts != 0 || captured.ends != 0 {
		t.Fatalf("disabled parser exposed reasoning: %+v", captured)
	}
	if got := captured.answer.String(); got != "answer" {
		t.Fatalf("answer=%q, want answer", got)
	}
}

func TestThinkingStreamParserFlushesIncompleteTagWithoutLeakingMarkup(t *testing.T) {
	var captured parserCapture
	parser := newThinkingStreamParser(true, captured.emit)
	parser.Push("<thinking>partial reason", false)
	parser.Flush()

	if got := captured.reasoning.String(); got != "partial reason" {
		t.Fatalf("reasoning=%q, want partial reason", got)
	}
	if captured.answer.Len() != 0 || captured.starts != 1 || captured.ends != 1 {
		t.Fatalf("unexpected capture: %+v", captured)
	}
}
