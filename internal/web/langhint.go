package web

import (
	"strconv"
	"strings"
)

// supportedLangs: the shell's languages — "en" plus UILangs (the same
// list i18n.js LANGS names; a language added there shows up here).
var supportedLangs = append([]string{"en"}, UILangs...)

// langHint maps a request's Accept-Language header onto the best UI
// language the shell can show: the highest-weighted tag that names a
// supported language, q=0 and unparseable parts excluded, the header's
// order breaking ties. "" when nothing matches — the page then follows
// the browser's own language (navigator.languages).
func langHint(acceptLanguage string) string {
	bestQ := 0.0
	best := ""
	for part := range strings.SplitSeq(acceptLanguage, ",") {
		tag, params, _ := strings.Cut(part, ";")
		code := matchLang(strings.ToLower(strings.TrimSpace(tag)))
		q := weight(params)
		if code == "" || q <= 0 {
			continue
		}
		if best == "" || q > bestQ { // ties keep the earlier tag
			best, bestQ = code, q
		}
	}
	return best
}

// weight reads one Accept-Language part's q parameter (1.0 when it
// carries none).
func weight(params string) float64 {
	for p := range strings.SplitSeq(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return 1.0
}

// matchLang maps one language tag onto a supported code ("en"… or
// "zh-Hans"), "" when the tag names no supported language: an exact
// code, any "zh" variant (→ zh-Hans), or the base tag before the first
// - or _ (de-AT → de).
func matchLang(tag string) string {
	if tag == "" {
		return ""
	}
	if tag == "zh" || strings.HasPrefix(tag, "zh-") || strings.HasPrefix(tag, "zh_") {
		return "zh-Hans"
	}
	if i := strings.IndexAny(tag, "-_"); i > 0 {
		tag = tag[:i]
	}
	for _, c := range supportedLangs {
		if c == tag {
			return c
		}
	}
	return ""
}
