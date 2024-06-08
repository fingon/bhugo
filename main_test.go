package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"sync"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
)

func TestUpdateHugo(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		in      note
		exp     []byte
		cleanup bool
	}{
		{
			"basic",
			"note-title/index.md",
			note{
				Title: "Note Title",
				BodyRaw: []byte(`# Note Title
#blog/tag

Body text`),
			},
			[]byte(`---
title: "Note Title"
date: 2001-01-01
categories: ["Tag"]
tags: ["Tag"]
draft: false
---

Body text`),
			true,
		},
		// Should preserve custom front matter of an existing note.
		{
			"existing note",
			"existing/index.md",
			note{
				Title: "Existing",
				BodyRaw: []byte(`# Existing
#blog/tag

Updated text`),
			},
			[]byte(`---
title: "Existing"
date: 2001-01-01
categories: ["Tag"]
tags: ["Tag"]
draft: false
custom: abc
---

Updated text`),
			false,
		},
	}

	cfg := config{
		NoteTag:              "blog",
		HugoDir:              "./testData/site",
		ContentDir:           "content",
		Categories:           true,
		Tags:                 true,
		TagLine:              1,
		OmitNonNoteTagPrefix: false,
		TimeFormat:           "2006-02-01",
	}

	tmpl, err := template.New("Note Template").Parse(templateRaw)
	require.NoError(t, err)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// channels are buffered so we can feed in stuff immediately
			done := make(chan bool, 1)
			done <- true

			notes := make(chan note, 1)
			notes <- test.in

			dir := fmt.Sprintf("%s/%s/%s", cfg.HugoDir, cfg.ContentDir, test.file)

			// Keep a copy of the original file if it exists.
			orig, _ := ioutil.ReadFile(dir)

			defer func() {
				if test.cleanup {
					err = os.Remove(dir)
					require.NoError(t, err)
				} else {
					err := ioutil.WriteFile(dir, orig, 0o666)
					require.NoError(t, err)
				}
			}()

			wg := sync.WaitGroup{}
			wg.Add(1)

			updateHugo(nil, &wg, done, notes, &cfg, tmpl)

			f, err := ioutil.ReadFile(dir)
			require.NoError(t, err)
			// TODO: If someone in US really wants to toy
			// with this, the Core Data epoch might roll to
			// 2000-12-31 as it is dumped tz-aware by
			// default..
			require.Equal(t, string(test.exp), string(f))
		})
	}
}

func TestScanTags(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		omit bool
		exp  []string
	}{
		{
			"empty",
			[]byte(""),
			false,
			[]string{},
		},
		{
			"one tag",
			[]byte("#prefix/abc"),
			false,
			[]string{"Abc"},
		},
		{
			"multi-word tag",
			[]byte("#prefix/abc def#"),
			false,
			[]string{"Abc Def"},
		},
		{
			"multiple tags",
			[]byte("#prefix/abc #prefix/def abc#  #def"),
			false,
			[]string{"Abc", "Def Abc", "Def"},
		},
		{
			"not hashes",
			[]byte("1234"),
			false,
			[]string{},
		},
		{
			"some hashes with some random text",
			[]byte("#prefix/abc 123 #one 456"),
			false,
			[]string{"Abc", "One"},
		},
		{
			"some hashes with some random text",
			[]byte("#prefix/abc 123 #one 456"),
			true,
			[]string{"Abc"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := scanTags(test.in, "prefix", test.omit)
			require.Equal(t, test.exp, got)
		})
	}
}

func TestCustomFrontMatter(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		exp  []string
	}{
		{"empty", nil, []string{}},
		{
			"basic",
			[]byte(`---
title: "Existing"
date: 2019-04-29T07:55:21-07:00
draft: false
custom: abc
categories: ["blog"]
tags: ["custom-tag"]
custom-2: abcd
---

Body Text`),
			[]string{"custom: abc", "custom-2: abcd"},
		},
		{
			"no opening dash",
			[]byte(`title: "Existing"
date: 2019-04-29T07:55:21-07:00
draft: false
categories: ["blog"]
tags: ["custom-tag"]
custom: abc
---

Body Text`),
			[]string{},
		},
		{
			"no closing dash",
			[]byte(`---
title: "Existing"
date: 2019-04-29T07:55:21-07:00
draft: false
categories: ["blog"]
tags: ["custom-tag"]
custom: abc

Body Text`),
			[]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := customFrontMatter(test.in)
			require.Equal(t, test.exp, got)
		})
	}
}
