package bridge

import "testing"

func TestSplitPreview(t *testing.T) {
	cases := []struct{ in, text, action string }{
		{"Started the site.\nPreview: open\n\nSummary:\nYour page is up.", "Started the site.\n\nSummary:\nYour page is up.", "open"},
		{"  preview: Close\nSummary:\nHidden.", "Summary:\nHidden.", "close"},
		{"Preview: open\nPreview: close", "", "close"},
		{"Summary:\nThe preview: open question remains.", "Summary:\nThe preview: open question remains.", ""},
		{"Summary:\nNothing to show.", "Summary:\nNothing to show.", ""},
	}
	for _, c := range cases {
		text, action := splitPreview(c.in)
		if text != c.text || action != c.action {
			t.Errorf("splitPreview(%q) = %q, %q; want %q, %q", c.in, text, action, c.text, c.action)
		}
	}
}
