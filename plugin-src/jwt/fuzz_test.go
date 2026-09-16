package main

import "testing"

func FuzzParseToken(f *testing.F) {
	f.Add("eyJhbGciOiJSUzI1NiJ9.e30.AA")
	f.Add("not-a-token")
	f.Fuzz(func(t *testing.T, token string) {
		if len(token) > maxTokenLen {
			return
		}
		_, _, _, _, _ = parseToken(token)
	})
}

func FuzzValidateJSON(f *testing.F) {
	f.Add([]byte(`{"a":[1,true,null]}`))
	f.Add([]byte(`{"a":1,"a":2}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxDocumentSize {
			return
		}
		_ = validateJSON(data)
	})
}
