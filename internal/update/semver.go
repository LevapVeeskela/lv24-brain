package update

import (
	"strconv"
	"strings"
)

// CompareVersions compares two semantic versions. It returns -1 when a is
// older than b, 0 when they are equivalent, and 1 when a is newer. A leading
// "v" and build metadata are ignored; prereleases sort before the release.
func CompareVersions(a, b string) int {
	aCore, aPre := splitVersion(a)
	bCore, bPre := splitVersion(b)
	if cmp := compareNumericCore(aCore, bCore); cmp != 0 {
		return cmp
	}
	return comparePrerelease(aPre, bPre)
}

func normalizeVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	value = strings.TrimPrefix(value, "V")
	return value
}

func splitVersion(value string) (string, string) {
	value = normalizeVersion(value)
	if idx := strings.IndexByte(value, '+'); idx >= 0 {
		value = value[:idx]
	}
	if idx := strings.IndexByte(value, '-'); idx >= 0 {
		return value[:idx], value[idx+1:]
	}
	return value, ""
}

func compareNumericCore(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	count := len(aParts)
	if len(bParts) > count {
		count = len(bParts)
	}
	for i := 0; i < count; i++ {
		aPart := "0"
		if i < len(aParts) {
			aPart = aParts[i]
		}
		bPart := "0"
		if i < len(bParts) {
			bPart = bParts[i]
		}
		aNum, aOK := parseNumeric(aPart)
		bNum, bOK := parseNumeric(bPart)
		if aOK && bOK {
			switch {
			case aNum < bNum:
				return -1
			case aNum > bNum:
				return 1
			}
			continue
		}
		if cmp := strings.Compare(aPart, bPart); cmp != 0 {
			return cmp
		}
	}
	return 0
}

func comparePrerelease(a, b string) int {
	if a == "" && b == "" {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	count := len(aParts)
	if len(bParts) < count {
		count = len(bParts)
	}
	for i := 0; i < count; i++ {
		aNum, aOK := parseNumeric(aParts[i])
		bNum, bOK := parseNumeric(bParts[i])
		if aOK && bOK {
			switch {
			case aNum < bNum:
				return -1
			case aNum > bNum:
				return 1
			}
			continue
		}
		if aOK != bOK {
			if aOK {
				return -1
			}
			return 1
		}
		if cmp := strings.Compare(aParts[i], bParts[i]); cmp != 0 {
			return cmp
		}
	}
	switch {
	case len(aParts) < len(bParts):
		return -1
	case len(aParts) > len(bParts):
		return 1
	default:
		return 0
	}
}

func parseNumeric(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return number, true
}
