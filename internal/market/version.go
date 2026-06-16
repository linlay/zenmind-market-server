package market

import "strings"

func canonicalVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == 'v' || value[0] == 'V') && value[1] >= '0' && value[1] <= '9' {
		return value[1:]
	}
	return value
}
