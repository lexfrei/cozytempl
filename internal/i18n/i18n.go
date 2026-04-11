// Package i18n wires cozytempl's localisation layer.
//
// The design deliberately avoids reaching into context from inside
// templ templates. Templates take a *Localizer as a parameter (via
// a view data struct) and call L.T("message.id") for every piece
// of user-visible text. The middleware attaches a per-request
// Localizer to the request context so handlers can pull it out and
// pass it along.
//
// Locale detection, in order of precedence:
//
//  1. The `cozytempl-lang` cookie (user explicitly picked a language).
//  2. The `Accept-Language` request header (browser preference).
//  3. The default locale (English).
//
// Supported locales are declared in SupportedLocales — adding a new
// language means shipping a new locales/active.<tag>.toml file and
// appending its tag here. Messages missing from a non-English file
// fall back through go-i18n's matcher chain to English.
//
// Message files use TOML because it's less syntactically noisy than
// JSON for long strings with punctuation, and go-i18n supports it
// natively.
package i18n

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"

	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/text/language"
)

// SupportedLocales enumerates every language cozytempl ships.
// Order matters for the matcher: the first entry is the default
// used when nothing else matches. English is the canonical source
// of truth for message IDs.
//
//nolint:gochecknoglobals // locale tag set is effectively const
var SupportedLocales = []language.Tag{
	language.English, // en — default / fallback
	language.Russian, // ru
	language.Kazakh,  // kk
	language.Chinese, // zh (simplified, matches zh, zh-CN, zh-Hans)
}

// cookieName is the persistent user-choice cookie. Short, neutral
// scheme under the cozytempl-* namespace alongside cozytempl-theme.
const cookieName = "cozytempl-lang"

// cookieMaxAge is the lifetime of the locale cookie, one year.
// A stable preference shouldn't need re-affirming every session.
const cookieMaxAge = 365 * 24 * 60 * 60

// translationsFS holds the embedded TOML message files. The
// locales/ directory is at compile time and the binary never needs
// to read anything from the filesystem at runtime.
//
//go:embed locales/*.toml
var translationsFS embed.FS

// Bundle wraps a loaded go-i18n bundle so the rest of the app only
// deals with our own types. Kept small on purpose — the NewBundle
// function does the heavy lifting once at startup.
type Bundle struct {
	goBundle *goi18n.Bundle
	matcher  language.Matcher
}

// NewBundle loads every message file under locales/ and returns a
// Bundle ready to hand out Localizers. Returns an error if a file
// fails to parse so the operator gets a clear failure at startup
// instead of silently missing translations.
func NewBundle() (*Bundle, error) {
	bundle := goi18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)

	for _, tag := range SupportedLocales {
		filename := "locales/active." + tag.String() + ".toml"

		data, err := translationsFS.ReadFile(filename)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, fmt.Errorf("loading %s: %w", filename, err)
			}
			// Missing non-English files are tolerated so a
			// partial translation ships; the matcher falls
			// through to English for any missing key. But we
			// require at least English.
			if tag == language.English {
				return nil, fmt.Errorf("loading required locale %s: %w", filename, err)
			}

			continue
		}

		_, parseErr := bundle.ParseMessageFileBytes(data, filename)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing %s: %w", filename, parseErr)
		}
	}

	return &Bundle{
		goBundle: bundle,
		matcher:  language.NewMatcher(SupportedLocales),
	}, nil
}

// Localizer is the per-request translator handed to templates and
// handlers. It wraps go-i18n's Localizer with a simple .T() helper
// that returns the message for the given ID (or the ID itself
// wrapped in brackets so a missing key is visible, never silent).
type Localizer struct {
	inner *goi18n.Localizer
	tag   language.Tag
}

// Tag returns the resolved language tag — used by the
// language-switcher UI to mark the active option.
func (loc *Localizer) Tag() language.Tag {
	return loc.tag
}

// T returns the translated string for messageID. If the message
// is missing we return "[id]" instead of an empty string so the
// omission is visible in review. Template data is passed through
// to go-i18n's text/template substitution for {{.Name}}-style
// placeholders.
func (loc *Localizer) T(messageID string, templateData ...map[string]any) string {
	cfg := &goi18n.LocalizeConfig{MessageID: messageID}
	if len(templateData) > 0 {
		cfg.TemplateData = templateData[0]
	}

	out, err := loc.inner.Localize(cfg)
	if err != nil {
		return "[" + messageID + "]"
	}

	return out
}

// localizerKey is the context key for the request Localizer. An
// empty struct keeps the value namespaced away from any other
// package's context values.
type localizerKey struct{}

// LocalizerFromContext returns the Localizer attached by Middleware,
// or a fallback English Localizer if the caller somehow bypassed
// the middleware. The fallback keeps tests and ad-hoc renderers
// working without a dedicated setup step — the bundle must still
// be initialised once, obviously.
func (b *Bundle) LocalizerFromContext(ctx context.Context) *Localizer {
	if loc, ok := ctx.Value(localizerKey{}).(*Localizer); ok {
		return loc
	}

	return &Localizer{
		inner: goi18n.NewLocalizer(b.goBundle, language.English.String()),
		tag:   language.English,
	}
}

// ContextWithLocalizer attaches a Localizer to ctx. Exposed for
// tests that need to simulate a request-like context.
func ContextWithLocalizer(ctx context.Context, loc *Localizer) context.Context {
	return context.WithValue(ctx, localizerKey{}, loc)
}

// FromContext returns the Localizer attached to ctx by
// Middleware. Unlike (*Bundle).LocalizerFromContext, this helper
// does NOT need a Bundle reference — it is intended for templ
// components that want to resolve strings without having a
// Bundle threaded into every template function. When no
// localizer is on ctx the function returns nil; callers should
// route through partial.Tc which handles the nil case by
// echoing the message id in brackets.
func FromContext(ctx context.Context) *Localizer {
	if ctx == nil {
		return nil
	}

	loc, _ := ctx.Value(localizerKey{}).(*Localizer)

	return loc
}

// Middleware builds a per-request Localizer and stashes it on the
// request context. Every downstream handler (and every templ
// template via the handler's view data) can pull it out with
// LocalizerFromContext.
func (b *Bundle) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		loc := b.resolve(req)
		ctx := ContextWithLocalizer(req.Context(), loc)

		next.ServeHTTP(writer, req.WithContext(ctx))
	})
}

// resolve picks a locale for the request using cookie → header →
// default. The language.Matcher does the heavy lifting; we only
// feed it the inputs we trust.
func (b *Bundle) resolve(req *http.Request) *Localizer {
	var candidates []string

	cookie, cookieErr := req.Cookie(cookieName)
	if cookieErr == nil {
		candidates = append(candidates, cookie.Value)
	}

	if accept := req.Header.Get("Accept-Language"); accept != "" {
		candidates = append(candidates, accept)
	}

	matched, _ := language.MatchStrings(b.matcher, candidates...)

	return &Localizer{
		inner: goi18n.NewLocalizer(b.goBundle, matched.String()),
		tag:   matched,
	}
}

// SetLocaleCookie writes the user's picked locale as a persistent
// cookie. Called by the language-switcher POST handler.
//
// Secure is set only when the inbound request was itself over
// HTTPS — detected either via req.TLS (direct TLS termination at
// the Go server) or via the X-Forwarded-Proto header (a trusted
// reverse proxy / ingress doing the TLS work). Setting Secure
// unconditionally would silently drop the cookie on http://localhost
// dev port-forwards, which is exactly how operators first smoke-test
// a fresh deployment — and that was the "language switch does
// nothing" bug on the live dev9 install.
//
// The cookie is not security-sensitive (it only carries the user's
// language preference) so dropping Secure on HTTP is not a real
// regression versus the previous always-Secure posture.
func SetLocaleCookie(writer http.ResponseWriter, req *http.Request, tag language.Tag) {
	http.SetCookie(writer, &http.Cookie{
		Name:     cookieName,
		Value:    tag.String(),
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		Secure:   isHTTPS(req),
		SameSite: http.SameSiteLaxMode,
	})
}

// isHTTPS reports whether the inbound request was served over
// TLS, either terminated directly by the Go server or by a
// trusted upstream proxy that sets X-Forwarded-Proto.
func isHTTPS(req *http.Request) bool {
	if req == nil {
		return false
	}

	if req.TLS != nil {
		return true
	}

	return req.Header.Get("X-Forwarded-Proto") == "https"
}

// LookupSupported parses the given language string and returns
// the matching entry from SupportedLocales, plus a boolean for
// whether the match succeeded. Used by the language-switcher
// handler to validate user input before writing the cookie —
// we only persist tags we can actually serve.
func LookupSupported(raw string) (language.Tag, bool) {
	parsed, err := language.Parse(raw)
	if err != nil {
		return language.English, false
	}

	parsedBase, _ := parsed.Base()

	for _, tag := range SupportedLocales {
		tagBase, _ := tag.Base()
		if tagBase == parsedBase {
			return tag, true
		}
	}

	return language.English, false
}
