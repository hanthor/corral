package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/tuna-os/corral/pkg/config"
	"gopkg.in/yaml.v3"
)

// ThemeConfig is the assembled theme — CLI flags override config.yaml, which
// overrides the built-in defaults.
type ThemeConfig struct {
	Accent        string `json:"accent"`
	Accent2       string `json:"accent_2"`
	BrandTitle    string `json:"brand_title"`
	BrandEmoji    string `json:"brand_emoji"`
	BrandSubtitle string `json:"brand_subtitle"`
	CustomCSS     string `json:"custom_css"`
}

// Built-in defaults. The empty BrandEmoji sentinel means "show the emoji";
// only an explicit empty-string from the user suppresses it.
var themeDefaults = ThemeConfig{
	Accent:        "#f0883e",
	Accent2:       "#d9742e",
	BrandTitle:    "Corral",
	BrandEmoji:    "\U0001F920", // 🤠
	BrandSubtitle: "Virtual Environment",
}

var (
	activeTheme ThemeConfig
	cliTheme    ThemeConfig // CLI overrides saved separately so they win over config file
)

func init() {
	activeTheme = themeDefaults
}

// SetCLITheme records CLI flag overrides. Called before Serve.
func SetCLITheme(accent, brandTitle, brandEmoji, brandSubtitle, customCSS string) {
	cliTheme.Accent = accent
	cliTheme.BrandTitle = brandTitle
	cliTheme.BrandEmoji = brandEmoji
	cliTheme.BrandSubtitle = brandSubtitle
	if customCSS != "" {
		data, err := os.ReadFile(customCSS)
		if err == nil {
			cliTheme.CustomCSS = strings.TrimSpace(string(data))
		}
	}
}

// applyCLITheme applies CLI overrides on top of the current theme.
// Called after loadThemeFromConfig so CLI flags always win.
func applyCLITheme() {
	if cliTheme.Accent != "" {
		activeTheme.Accent = cliTheme.Accent
		activeTheme.Accent2 = darkenHex(cliTheme.Accent)
	}
	if cliTheme.BrandTitle != "" {
		activeTheme.BrandTitle = cliTheme.BrandTitle
	}
	if cliTheme.BrandEmoji != "" {
		activeTheme.BrandEmoji = cliTheme.BrandEmoji
	}
	if cliTheme.BrandSubtitle != "" {
		activeTheme.BrandSubtitle = cliTheme.BrandSubtitle
	}
	if cliTheme.CustomCSS != "" {
		activeTheme.CustomCSS = cliTheme.CustomCSS
	}
}

// loadThemeFromConfig reads config.yaml and applies non-zero values on top of
// the current theme (which may already include CLI overrides — those win). Only
// fields that haven't already been set by CLI flags are applied.
func loadThemeFromConfig(cfg *config.Config) {
	if cfg.Web.Accent != "" {
		activeTheme.Accent = cfg.Web.Accent
		activeTheme.Accent2 = darkenHex(cfg.Web.Accent)
	}
	if cfg.Web.Accent2 != "" {
		activeTheme.Accent2 = cfg.Web.Accent2
	}
	if cfg.Web.BrandTitle != "" {
		activeTheme.BrandTitle = cfg.Web.BrandTitle
	}
	if cfg.Web.BrandEmoji != "" {
		activeTheme.BrandEmoji = cfg.Web.BrandEmoji
	}
	if cfg.Web.BrandSubtitle != "" {
		activeTheme.BrandSubtitle = cfg.Web.BrandSubtitle
	}
	if cfg.Web.CustomCSS != "" {
		activeTheme.CustomCSS = cfg.Web.CustomCSS
	}
}

// darkenHex returns a slightly darkened variant of a CSS hex colour string by
// reducing each RGB component by ~12%. Returns the original for invalid input.
func darkenHex(hex string) string {
	h := strings.TrimPrefix(hex, "#")
	if len(h) != 6 {
		return hex
	}
	r, g, b := h[0:2], h[2:4], h[4:6]
	dim := func(c string) string {
		v := int(hexToByte(c))
		v = v * 7 / 8 // ~87.5%
		return fmt.Sprintf("%02x", v)
	}
	return "#" + dim(r) + dim(g) + dim(b)
}

func hexToByte(hex string) byte {
	v := byte(0)
	for _, c := range []byte(hex) {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v += c - '0'
		case c >= 'a' && c <= 'f':
			v += c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v += c - 'A' + 10
		}
	}
	return v
}

// themeStyle returns the <style> block that overrides CSS custom properties and
// injects custom CSS. Safe to include in <head>.
func (t ThemeConfig) themeStyle() string {
	var b strings.Builder
	b.WriteString("<style id=\"corral-theme\">\n")
	b.WriteString(":root {\n")
	fmt.Fprintf(&b, "  --accent: %s;\n", t.Accent)
	fmt.Fprintf(&b, "  --accent-2: %s;\n", t.Accent2)
	b.WriteString("}\n")
	if t.CustomCSS != "" {
		b.WriteString(t.CustomCSS)
		b.WriteString("\n")
	}
	b.WriteString("</style>")
	return b.String()
}

// injectTheme rewrites the embedded index.html with the active theme.
func injectTheme(html string) string {
	// Inject the theme <style> block before </head>.
	html = strings.Replace(html, "</head>", activeTheme.themeStyle()+"\n</head>", 1)

	// Branding: replace the hardcoded header content.
	oldBrand := `<div class="brand">🤠 <strong>Corral</strong> <span>Virtual Environment</span></div>`
	emojiSpan := ""
	if activeTheme.BrandEmoji != "" {
		emojiSpan = activeTheme.BrandEmoji + " "
	}
	newBrand := fmt.Sprintf(`<div class="brand">%s<strong>%s</strong> <span>%s</span></div>`,
		emojiSpan, activeTheme.BrandTitle, activeTheme.BrandSubtitle)
	html = strings.Replace(html, oldBrand, newBrand, 1)

	return html
}

// handleGetTheme returns the current theme as JSON.
func handleGetTheme(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, http.StatusOK, activeTheme)
}

// handlePutTheme accepts a partial theme JSON, merges non-zero values into
// config.yaml, and writes it back. The active theme is updated immediately.
func handlePutTheme(w http.ResponseWriter, r *http.Request) {
	var req ThemeConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}

	cfgPath := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		errResp(w, http.StatusInternalServerError, fmt.Errorf("reading config: %w", err))
		return
	}

	// Merge incoming values into cfg.Web (non-zero only).
	if req.Accent != "" {
		cfg.Web.Accent = req.Accent
	}
	if req.Accent2 != "" {
		cfg.Web.Accent2 = req.Accent2
	}
	if req.BrandTitle != "" {
		cfg.Web.BrandTitle = req.BrandTitle
	}
	if req.BrandEmoji != "" {
		cfg.Web.BrandEmoji = req.BrandEmoji
	}
	if req.BrandSubtitle != "" {
		cfg.Web.BrandSubtitle = req.BrandSubtitle
	}
	if req.CustomCSS != "" {
		cfg.Web.CustomCSS = req.CustomCSS
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		errResp(w, http.StatusInternalServerError, fmt.Errorf("marshalling config: %w", err))
		return
	}
	if err := os.MkdirAll(config.ConfigDir(), 0o700); err != nil {
		errResp(w, http.StatusInternalServerError, fmt.Errorf("creating config dir: %w", err))
		return
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		errResp(w, http.StatusInternalServerError, fmt.Errorf("writing config: %w", err))
		return
	}

	// Update the active theme so the change takes effect on next page load.
	loadThemeFromConfig(cfg)
	jsonResp(w, http.StatusOK, activeTheme)
}
