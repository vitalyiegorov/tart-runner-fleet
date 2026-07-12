package telemetry

import "strings"

const redacted = "[REDACTED]"

var sensitive = map[string]struct{}{
	"authorization": {}, "token": {}, "password": {}, "secret": {},
	"jitconfig": {}, "privatekey": {}, "clientsecret": {},
}

func Redact(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		normal := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
		if _, ok := sensitive[normal]; ok {
			out[key] = redacted
			continue
		}
		out[key] = redactValue(value)
	}
	return out
}

func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return Redact(v)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = redactValue(v[i])
		}
		return out
	default:
		return value
	}
}
