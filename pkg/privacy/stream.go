package privacy

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type streamFrame struct {
	prefix, data []byte
}

type streamLane struct {
	pending         string
	frame           *streamFrame
	path            string
	quoted, escaped bool // position in serialized tool-argument JSON
}

// Stream restores decoded SSE deltas, not raw network chunks. This prevents
// split references and JSON escaping from corrupting client-visible text/tools.
type Stream struct {
	Session      *Session
	RestoreTools bool
	input        []byte
	frames       []*streamFrame
	lanes        map[string]*streamLane
	buffered     int
}

func (s *Stream) Write(data []byte, final bool) ([]byte, error) {
	s.input = append(s.input, data...)
	if len(s.input)+s.buffered > 4<<20 {
		return nil, ErrLimit
	}
	var out bytes.Buffer
	for {
		end := bytes.Index(s.input, []byte("\n\n"))
		width := 2
		if crlf := bytes.Index(s.input, []byte("\r\n\r\n")); crlf >= 0 && (end < 0 || crlf < end) {
			end, width = crlf, 4
		}
		if end < 0 {
			break
		}
		frame := append([]byte(nil), s.input[:end]...)
		s.input = s.input[end+width:]
		if err := s.accept(frame); err != nil {
			return nil, err
		}
		if !s.hasPending() {
			out.Write(s.drain())
		}
	}
	if final {
		if len(bytes.TrimSpace(s.input)) > 0 {
			if err := s.accept(s.input); err != nil {
				return nil, err
			}
		}
		s.input = nil
		if err := s.finishLanes(); err != nil {
			return nil, err
		}
		out.Write(s.drain())
	}
	return out.Bytes(), nil
}

func (s *Stream) accept(raw []byte) error {
	frame := &streamFrame{}
	var data []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		} else {
			frame.prefix = append(frame.prefix, []byte(line+"\n")...)
		}
	}
	if len(data) > 0 {
		frame.data = []byte(strings.Join(data, "\n"))
	}
	if len(frame.data) > 0 && string(frame.data) != "[DONE]" {
		if !gjson.ValidBytes(frame.data) {
			return fmt.Errorf("invalid privacy SSE JSON")
		}
		if s.lanes == nil {
			s.lanes = make(map[string]*streamLane)
		}
		root := gjson.ParseBytes(frame.data)
		typ := root.Get("type").String()
		var err error
		// Full-value fields (including *.done and response.completed) are restored
		// normally; deltas below use the original value and per-content state.
		frame.data, err = s.Session.RestoreJSON(frame.data, s.RestoreTools)
		if err != nil {
			return err
		}
		for i, choice := range root.Get("choices").Array() {
			base := fmt.Sprintf("choices.%d.delta.", i)
			lane := fmt.Sprintf("chat:%d:", choice.Get("index").Int())
			for _, field := range []string{"content", "reasoning_content", "reasoning"} {
				if err := s.delta(frame, base+field, lane+field, choice.Get("delta."+field), false); err != nil {
					return err
				}
			}
			if s.RestoreTools {
				for j, tool := range choice.Get("delta.tool_calls").Array() {
					path := fmt.Sprintf("%stool_calls.%d.function.arguments", base, j)
					if err := s.delta(frame, path, fmt.Sprintf("%stool:%d", lane, tool.Get("index").Int()), tool.Get("function.arguments"), true); err != nil {
						return err
					}
				}
				if err := s.delta(frame, base+"function_call.arguments", lane+"function", choice.Get("delta.function_call.arguments"), true); err != nil {
					return err
				}
			}
		}
		if typ == "content_block_delta" {
			for _, field := range []string{"text", "thinking", "partial_json"} {
				if field == "partial_json" && !s.RestoreTools {
					continue
				}
				if err := s.delta(frame, "delta."+field, fmt.Sprintf("claude:%d:%s", root.Get("index").Int(), field), root.Get("delta."+field), field == "partial_json"); err != nil {
					return err
				}
			}
		}
		if strings.HasPrefix(typ, "response.") && strings.HasSuffix(typ, ".delta") {
			isTool := strings.Contains(typ, "function_call_arguments") || strings.Contains(typ, "custom_tool_call_input")
			if !isTool || s.RestoreTools {
				lane := fmt.Sprintf("responses:%s:%d:%d:%d", typ, root.Get("output_index").Int(), root.Get("content_index").Int(), root.Get("summary_index").Int())
				if err := s.delta(frame, "delta", lane, root.Get("delta"), strings.Contains(typ, "function_call_arguments")); err != nil {
					return err
				}
			}
		}
		for i, candidate := range root.Get("candidates").Array() {
			for j, part := range candidate.Get("content.parts").Array() {
				path := fmt.Sprintf("candidates.%d.content.parts.%d.text", i, j)
				if err := s.delta(frame, path, fmt.Sprintf("gemini:%d:%d", candidate.Get("index").Int(), j), part.Get("text"), false); err != nil {
					return err
				}
			}
		}
		if typ == "message_stop" || typ == "response.completed" || typ == "response.failed" || typ == "error" {
			if err := s.finishLanes(); err != nil {
				return err
			}
		}
	}
	if string(frame.data) == "[DONE]" {
		if err := s.finishLanes(); err != nil {
			return err
		}
	}
	s.frames = append(s.frames, frame)
	s.buffered += len(raw)
	if len(s.frames) > 512 || s.buffered > 4<<20 {
		return ErrLimit
	}
	return nil
}

func (s *Stream) delta(frame *streamFrame, path, key string, value gjson.Result, jsonArgument bool) error {
	if value.Type != gjson.String {
		return nil
	}
	lane := s.lanes[key]
	if lane == nil {
		lane = &streamLane{}
		s.lanes[key] = lane
	}
	input := lane.pending + value.String()
	lane.pending = ""
	values := s.Session.snapshot()
	var output strings.Builder
	for i := 0; i < len(input); {
		rest := input[i:]
		partial := false
		replaced := false
		for ref, secret := range values {
			if strings.HasPrefix(ref, rest) && len(rest) < len(ref) {
				partial = true
				break
			}
			if strings.HasPrefix(rest, ref) {
				if jsonArgument {
					// Inside a JSON string, quotes/backslashes/newlines in a secret
					// need one extra escaping layer before the outer SSE encoding.
					if lane.quoted && !lane.escaped {
						encoded, _ := common.Marshal(secret)
						output.Write(encoded[1 : len(encoded)-1])
					} else {
						output.WriteString(ref)
					}
				} else {
					output.WriteString(secret)
				}
				i += len(ref)
				replaced = true
				break
			}
		}
		if partial {
			lane.pending = rest
			break
		}
		if replaced {
			continue
		}
		b := input[i]
		output.WriteByte(b)
		if jsonArgument {
			if lane.escaped {
				lane.escaped = false
			} else if lane.quoted && b == '\\' {
				lane.escaped = true
			} else if b == '"' {
				lane.quoted = !lane.quoted
			}
		}
		i++
	}
	changed, err := sjson.SetBytes(frame.data, path, output.String())
	if err != nil {
		return fmt.Errorf("privacy delta encoding failed")
	}
	frame.data = changed
	lane.frame, lane.path = frame, path
	return nil
}

func (s *Stream) hasPending() bool {
	for _, lane := range s.lanes {
		if lane.pending != "" {
			return true
		}
	}
	return false
}

func (s *Stream) finishLanes() error {
	for _, lane := range s.lanes {
		if lane.pending == "" || lane.frame == nil {
			continue
		}
		value := gjson.GetBytes(lane.frame.data, lane.path).String() + lane.pending
		data, err := sjson.SetBytes(lane.frame.data, lane.path, value)
		if err != nil {
			return fmt.Errorf("privacy terminal encoding failed")
		}
		lane.frame.data, lane.pending = data, ""
	}
	return nil
}

func (s *Stream) drain() []byte {
	var out bytes.Buffer
	for _, frame := range s.frames {
		out.Write(frame.prefix)
		if frame.data != nil {
			out.WriteString("data: ")
			out.Write(frame.data)
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}
	s.frames, s.buffered = nil, 0
	return out.Bytes()
}
