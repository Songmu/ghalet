// Package logreduct masks registered secrets in log text and slog attributes.
package logreduct

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
)

// Redactor tracks secrets and their reference counts for log masking.
// The zero value is ready to use. A Redactor is safe for concurrent use
// and must not be copied after first use.
type Redactor struct {
	mu     sync.RWMutex
	values []string
	refs   map[string]int
}

func secretComponents(value string) []string {
	if value == "" {
		return nil
	}
	values := []string{value}
	if strings.Contains(value, "\n") {
		for line := range strings.SplitSeq(value, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.TrimSpace(line) != "" {
				values = append(values, line)
			}
		}
	}
	return values
}

// Add registers a secret and its nonblank lines for masking.
func (s *Redactor) Add(value string) {
	if value == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs == nil {
		s.refs = map[string]int{}
	}
	for _, component := range secretComponents(value) {
		if s.refs[component] == 0 {
			s.values = append(s.values, component)
		}
		s.refs[component]++
	}
}

// Redact replaces registered secrets with "***", merging overlapping matches.
func (s *Redactor) Redact(value string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Bound match storage by input size, even with many overlapping matches.
	var ends []int
	for _, secret := range s.values {
		for offset := 0; offset < len(value); {
			match := strings.Index(value[offset:], secret)
			if match < 0 {
				break
			}
			start := offset + match
			if ends == nil {
				ends = make([]int, len(value))
			}
			ends[start] = max(ends[start], start+len(secret))
			offset = start + 1
		}
	}
	if ends == nil {
		return value
	}
	var output strings.Builder
	last := 0
	for start := 0; start < len(value); start++ {
		end := ends[start]
		if end == 0 {
			continue
		}
		output.WriteString(value[last:start])
		for next := start + 1; next < end; next++ {
			end = max(end, ends[next])
		}
		output.WriteString("***")
		last = end
		start = end - 1
	}
	output.WriteString(value[last:])
	return output.String()
}

// AddJIT registers an encoded GitHub Actions JIT configuration and any decoded
// content and credential fields. Pass the returned values to Release after use.
func (s *Redactor) AddJIT(value string) []string {
	values := []string{value}
	defer func() {
		for _, value := range values {
			s.Add(value)
		}
	}()
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return values
	}
	values = append(values, string(raw))
	var fields map[string]string
	if json.Unmarshal(raw, &fields) == nil {
		for _, field := range fields {
			values = append(values, field)
			if decoded, err := base64.StdEncoding.DecodeString(field); err == nil {
				values = append(values, string(decoded))
			}
		}
	}
	return values
}

// Release drops one registration of each value and its nonblank lines.
// A secret remains masked until all of its registrations have been released.
func (s *Redactor) Release(values []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range values {
		for _, component := range secretComponents(value) {
			if s.refs[component] > 1 {
				s.refs[component]--
				continue
			}
			delete(s.refs, component)
			for i, old := range s.values {
				if old == component {
					copy(s.values[i:], s.values[i+1:])
					s.values[len(s.values)-1] = ""
					s.values = s.values[:len(s.values)-1]
					break
				}
			}
		}
	}
}

// ReplaceAttr masks strings and errors, including nested groups, for use with
// slog.HandlerOptions.ReplaceAttr.
func (s *Redactor) ReplaceAttr(_ []string, attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindGroup {
		attrs := attr.Value.Group()
		redacted := make([]slog.Attr, len(attrs))
		for i, child := range attrs {
			redacted[i] = s.ReplaceAttr(nil, child)
		}
		attr.Value = slog.GroupValue(redacted...)
	}
	if attr.Value.Kind() == slog.KindString {
		attr.Value = slog.StringValue(s.Redact(attr.Value.String()))
	} else if attr.Value.Kind() == slog.KindAny {
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(s.Redact(err.Error()))
		}
	}
	return attr
}
