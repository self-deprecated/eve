package sizeguard

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatBytes renders n in decimal units with one decimal place: "512 B", "40.0 KB", "48.2 MB",
// "1.2 GB". Negative sizes keep their sign.
func FormatBytes(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	f := float64(n)
	switch {
	case n < 1000:
		return fmt.Sprintf("%s%d B", sign, n)
	case n < 1000*1000:
		return fmt.Sprintf("%s%.1f KB", sign, f/1e3)
	case n < 1000*1000*1000:
		return fmt.Sprintf("%s%.1f MB", sign, f/1e6)
	}
	return fmt.Sprintf("%s%.1f GB", sign, f/1e9)
}

// formatDelta renders a size change with an explicit sign: "+1.2 MB", "-400.0 KB", "+0 B".
func formatDelta(n int64) string {
	if n >= 0 {
		return "+" + FormatBytes(n)
	}
	return FormatBytes(n)
}

// formatCount renders n with thousands separators: "9,412".
func formatCount(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// formatCountDelta renders a count change with an explicit sign: "+9,412".
func formatCountDelta(n int) string {
	if n >= 0 {
		return "+" + formatCount(n)
	}
	return formatCount(n)
}

// percent renders the growth from base to cur, or "" when base is zero.
func percent(cur, base int64) string {
	if base <= 0 {
		return ""
	}
	return fmt.Sprintf("%+.0f%%", float64(cur-base)*100/float64(base))
}

// shorten caps s at n characters, eliding the middle so both ends of a path stay readable.
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 8 {
		return s
	}
	head := (n - 3) / 3
	tail := n - 3 - head
	return string(r[:head]) + "..." + string(r[len(r)-tail:])
}

// list joins items, keeping at most n and summarizing the rest.
func list(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}
