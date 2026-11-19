// SPDX-License-Identifier: MPL-2.0

package media

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOfferedTag pins which values of an a=mid line are taken as an
// identification-tag: the tokens of RFC 4566 section 9, and nothing a value
// could use to end its line or its field in an SDP of ours.
func TestOfferedTag(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  string
		want string
	}{
		{name: "digits", tag: "0", want: "0"},
		{name: "letters", tag: "audio", want: "audio"},
		{name: "every_token_punctuation", tag: "!#$%&'*+-.^_`{|}~", want: "!#$%&'*+-.^_`{|}~"},
		{name: "longer_than_an_extension_element_holds", tag: strings.Repeat("t", 300), want: strings.Repeat("t", 300)},
		{name: "empty"},
		{name: "space", tag: "a b"},
		{name: "trailing_space", tag: "a "},
		{name: "tab", tag: "a\tb"},
		{name: "carriage_return", tag: "a\ra=inactive"},
		{name: "line_feed", tag: "a\nb"},
		{name: "null", tag: "a\x00"},
		{name: "delete", tag: "a\x7f"},
		{name: "double_quote", tag: `a"b`},
		{name: "parenthesis", tag: "a(b"},
		{name: "comma", tag: "a,b"},
		{name: "slash", tag: "a/b"},
		{name: "colon", tag: "a:b"},
		{name: "equals", tag: "a=b"},
		{name: "at", tag: "a@b"},
		{name: "bracket", tag: "a[b"},
		{name: "backslash", tag: `a\b`},
		{name: "beyond_ASCII", tag: "caf\u00e9"},
		{name: "invalid_UTF-8", tag: "a\xffb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, offeredTag(tc.tag), "offeredTag(%q)", tc.tag)
		})
	}
}
