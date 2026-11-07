package hub

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits from SPEC.md section 8.
const (
	MaxTextBytes     = 65536
	MaxTopicBytes    = 256
	MaxMetaEntries   = 16
	MaxMetaKeyBytes  = 64
	MaxMetaValBytes  = 256
	MaxEmojiBytes    = 64
	MentionAll       = "all"
	DefaultChannel   = "general"
	defaultRetention = 10000
)

var (
	agentNameRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	channelNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// ValidateAgentName checks AGT-1.
func ValidateAgentName(name string) error {
	if !agentNameRE.MatchString(name) {
		return invalid("invalid agent name %q: must match [A-Za-z0-9_-]{1,64}", name)
	}
	if strings.EqualFold(name, MentionAll) {
		return invalid("agent name %q is reserved", name)
	}
	return nil
}

// ValidateChannelName checks CHN-1.
func ValidateChannelName(name string) error {
	if !channelNameRE.MatchString(name) {
		return invalid("invalid channel name %q: must match [a-z0-9][a-z0-9_-]{0,63}", name)
	}
	return nil
}

func validateText(text string) error {
	if !utf8.ValidString(text) {
		return invalid("text must be valid UTF-8")
	}
	if strings.TrimSpace(text) == "" {
		return invalid("text must not be empty")
	}
	if len(text) > MaxTextBytes {
		return invalid("text exceeds %d bytes", MaxTextBytes)
	}
	return nil
}

func validateMeta(meta map[string]string) error {
	if len(meta) > MaxMetaEntries {
		return invalid("meta has more than %d entries", MaxMetaEntries)
	}
	for k, v := range meta {
		if k == "" || len(k) > MaxMetaKeyBytes {
			return invalid("meta key %q must be 1-%d bytes", k, MaxMetaKeyBytes)
		}
		if len(v) > MaxMetaValBytes {
			return invalid("meta value for %q exceeds %d bytes", k, MaxMetaValBytes)
		}
	}
	return nil
}

func validateEmoji(emoji string) error {
	if emoji == "" || len(emoji) > MaxEmojiBytes || !utf8.ValidString(emoji) {
		return invalid("emoji must be 1-%d bytes of UTF-8", MaxEmojiBytes)
	}
	if strings.IndexFunc(emoji, unicode.IsSpace) >= 0 {
		return invalid("emoji must not contain whitespace")
	}
	return nil
}

func isNameByte(b byte) bool {
	return b == '_' || b == '-' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ParseMentions extracts @mentions from text according to MSG-3.
func ParseMentions(text string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		if i > 0 && (isNameByte(text[i-1]) || text[i-1] == '@') {
			continue
		}
		j := i + 1
		for j < len(text) && isNameByte(text[j]) {
			j++
		}
		name := text[i+1 : j]
		if name != "" && len(name) <= 64 && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}
