// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package sdp

// connectionValue returns the value of the c= line ConnectionInformation
// reads, and false when the body has none.
//
// RFC 4566 section 5.7 lets a c= inside a media section override the session
// level one for that media. The lines are kept by type, not by section, so a
// c= can be bound to its section only while the body carries a single m= line:
// there the last c= is the media level one when the section has its own, and
// the session level one otherwise.
//
// With more than one m= line the last c= may belong to another stream, so the
// first is returned: RFC 4566 section 5 fixes the order of the lines, which
// makes it the session level c= whenever the body has one, the default for
// every stream without its own.
func (sd SessionDescription) connectionValue() (string, bool) {
	values := sd.Values("c")
	if len(values) == 0 {
		return "", false
	}
	if len(sd.Values("m")) > 1 {
		return values[0], true
	}
	return values[len(values)-1], true
}
