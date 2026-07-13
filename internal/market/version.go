package market

import (
	"strconv"
	"strings"
)

func canonicalVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == 'v' || value[0] == 'V') && value[1] >= '0' && value[1] <= '9' {
		return value[1:]
	}
	return value
}

// compareSemanticVersions compares valid Semantic Version 2.0.0 values. It
// returns false when either value is malformed so malformed legacy rows can
// never be selected as a latest version automatically.
func compareSemanticVersions(left, right string) (int, bool) {
	l, ok := parseSemanticVersion(left)
	if !ok {
		return 0, false
	}
	r, ok := parseSemanticVersion(right)
	if !ok {
		return 0, false
	}
	for i := range l.core {
		if l.core[i] < r.core[i] {
			return -1, true
		}
		if l.core[i] > r.core[i] {
			return 1, true
		}
	}
	if l.pre == "" && r.pre != "" {
		return 1, true
	}
	if l.pre != "" && r.pre == "" {
		return -1, true
	}
	if l.pre == r.pre {
		return 0, true
	}
	lParts, rParts := strings.Split(l.pre, "."), strings.Split(r.pre, ".")
	for i := 0; i < len(lParts) && i < len(rParts); i++ {
		if lParts[i] == rParts[i] {
			continue
		}
		lNumber, lNumeric := numericIdentifier(lParts[i])
		rNumber, rNumeric := numericIdentifier(rParts[i])
		switch {
		case lNumeric && rNumeric:
			if lNumber < rNumber { return -1, true }
			return 1, true
		case lNumeric:
			return -1, true
		case rNumeric:
			return 1, true
		case lParts[i] < rParts[i]:
			return -1, true
		default:
			return 1, true
		}
	}
	if len(lParts) < len(rParts) { return -1, true }
	if len(lParts) > len(rParts) { return 1, true }
	return 0, true
}

type semanticVersion struct {
	core [3]int64
	pre  string
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = canonicalVersion(value)
	if value == "" { return semanticVersion{}, false }
	withoutBuild := strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(withoutBuild, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 3 { return semanticVersion{}, false }
	var result semanticVersion
	for i, part := range core {
		number, ok := numericIdentifier(part)
		if !ok || (len(part) > 1 && part[0] == '0') { return semanticVersion{}, false }
		result.core[i] = number
	}
	if len(parts) == 2 {
		if parts[1] == "" { return semanticVersion{}, false }
		for _, part := range strings.Split(parts[1], ".") {
			if part == "" || !validIdentifier(part) || (len(part) > 1 && part[0] == '0' && isDigits(part)) { return semanticVersion{}, false }
		}
		result.pre = parts[1]
	}
	return result, true
}

func numericIdentifier(value string) (int64, bool) {
	if value == "" || !isDigits(value) { return 0, false }
	number, err := strconv.ParseInt(value, 10, 64)
	return number, err == nil
}

func isDigits(value string) bool {
	for _, char := range value { if char < '0' || char > '9' { return false } }
	return value != ""
}

func validIdentifier(value string) bool {
	for _, char := range value {
		if (char >= '0' && char <= '9') || (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '-' { continue }
		return false
	}
	return true
}
