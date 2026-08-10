package writefreely

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/writeas/impart"
)

func TestValidateStyleSheetNil(t *testing.T) {
	assert.NoError(t, validateStyleSheet(nil))
}

func TestValidateStyleSheetAllows(t *testing.T) {
	for _, css := range []string{
		"",
		"body { color: red; }",
		".title { font-family: 'My Font', sans-serif; }",
		"body { background: url(/static/local/bg.png); }",
		"body { background: url('images/bg.png'); }",
		"body { background: url(data:image/png;base64,iVBORw0KGgo=); }",
	} {
		css := css
		t.Run(css, func(t *testing.T) {
			assert.NoError(t, validateStyleSheet(&css))
		})
	}
}

func TestValidateStyleSheetBlocksExternalRefs(t *testing.T) {
	for _, css := range []string{
		"@import url('https://evil.example/track.css');",
		"@import 'https://evil.example/track.css';",
		"body { background: url(http://evil.example/track.gif); }",
		"body { background: url(https://evil.example/track.gif); }",
		"body { background: url(//evil.example/track.gif); }",
		"body { background: url('//evil.example/track.gif'); }",
	} {
		css := css
		t.Run(css, func(t *testing.T) {
			err := validateStyleSheet(&css)
			if assert.Error(t, err) {
				httpErr, ok := err.(impart.HTTPError)
				if assert.True(t, ok, "expected impart.HTTPError, got %T", err) {
					assert.Equal(t, http.StatusBadRequest, httpErr.Status)
				}
			}
		})
	}
}

func TestValidateStyleSheetBlocksTooLong(t *testing.T) {
	css := strings.Repeat("a", styleSheetMaxLength+1)
	err := validateStyleSheet(&css)
	if assert.Error(t, err) {
		httpErr, ok := err.(impart.HTTPError)
		if assert.True(t, ok, "expected impart.HTTPError, got %T", err) {
			assert.Equal(t, http.StatusBadRequest, httpErr.Status)
		}
	}
}
