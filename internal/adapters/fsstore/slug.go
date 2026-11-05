package fsstore

import "strings"

const maxSlugBytes = 60

// cyrillicSlug maps Cyrillic letters to stable ASCII equivalents. The map covers
// Russian plus the Belarusian and Ukrainian letters common in LV24 project data.
var cyrillicSlug = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "i", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
	'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g", 'ў': "u",
}

// slugify turns a human title into a deterministic ASCII file-name stem.
// Letters outside the Latin alphabet and the Cyrillic map become dashes.
func slugify(value string) string {
	var out strings.Builder
	lastDash := true
	for _, char := range strings.ToLower(value) {
		mapped, ok := cyrillicSlug[char]
		if !ok {
			switch {
			case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
				mapped = string(char)
			default:
				mapped = "-"
			}
		}
		for _, slugChar := range mapped {
			if slugChar == '-' {
				if lastDash {
					continue
				}
				out.WriteByte('-')
				lastDash = true
				continue
			}
			out.WriteByte(byte(slugChar))
			lastDash = false
			if out.Len() >= maxSlugBytes {
				return strings.Trim(out.String(), "-")
			}
		}
	}
	slug := strings.Trim(out.String(), "-")
	if slug == "" {
		return "item"
	}
	return slug
}

// singleLine collapses a title into one line so it cannot break a Markdown
// heading layout.
func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
