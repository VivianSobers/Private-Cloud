package extract

import (
	"mime"
	"sort"
	"strings"
)

// Tags derives automatic tags for a file from its content type and extracted
// text. Deterministic and explainable by construction: a MIME-category tag every
// file gets, plus tags from a small fixed keyword vocabulary matched against the
// text. No model, no guessing — a user can look at any tag and see exactly why it
// is there, which is the whole point of keeping auto-tagging cheap.
func Tags(contentType, text string) []string {
	set := map[string]struct{}{}
	for _, t := range mimeTags(contentType) {
		set[t] = struct{}{}
	}
	for _, t := range keywordTags(text) {
		set[t] = struct{}{}
	}

	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// mimeTags maps a content type to broad category tags.
func mimeTags(contentType string) []string {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return []string{"image"}
	case strings.HasPrefix(mediaType, "video/"):
		return []string{"video"}
	case strings.HasPrefix(mediaType, "audio/"):
		return []string{"audio"}
	}

	switch mediaType {
	case "application/pdf":
		return []string{"document", "pdf"}
	case "application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/rtf":
		return []string{"document"}
	case "application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"text/csv", "application/csv":
		return []string{"spreadsheet"}
	case "application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return []string{"presentation"}
	case "application/zip", "application/x-tar", "application/gzip",
		"application/x-7z-compressed", "application/x-rar-compressed":
		return []string{"archive"}
	case "application/json", "application/xml", "application/x-ndjson",
		"application/yaml", "application/x-yaml", "application/toml":
		return []string{"data"}
	}

	if strings.HasPrefix(mediaType, "text/") {
		return []string{"text"}
	}
	return nil
}

// keywordVocabulary maps a tag to the trigger phrases that imply it. Small and
// conservative on purpose: a noisy tag is worse than a missing one, so this is a
// curated list, not top-frequency words.
var keywordVocabulary = map[string][]string{
	"invoice":   {"invoice", "invoice number", "invoice #"},
	"receipt":   {"receipt", "thank you for your purchase", "order confirmation"},
	"financial": {"subtotal", "amount due", "balance due", "sales tax", "total due"},
	"contract":  {"agreement", "hereby agree", "terms and conditions", "party of the first part"},
	"resume":    {"curriculum vitae", "work experience", "professional experience", "employment history"},
	"legal":     {"pursuant to", "confidential", "all rights reserved", "liability"},
}

// keywordTags returns the vocabulary tags whose triggers appear in the text.
func keywordTags(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lower := strings.ToLower(text)
	var out []string
	for tag, triggers := range keywordVocabulary {
		for _, trig := range triggers {
			if strings.Contains(lower, trig) {
				out = append(out, tag)
				break
			}
		}
	}
	return out
}
