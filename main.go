package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	sql "github.com/jmoiron/sqlx"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	_ "github.com/mattn/go-sqlite3"
	log "github.com/sirupsen/logrus"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type config struct {
	Interval   time.Duration `default:"1s"`
	HugoDir    string        `split_words:"true" default:"."`
	ContentDir string        `split_words:"true" default:"content/blog"`
	NoteTag    string        `split_words:"true" default:"blog"`
	Categories bool          `default:"true"`
	Tags       bool          `default:"false"`
	TimeFormat string        `default:"2006-01-02T15:04:05-07:00"`

	TagLine              int  `default:"-1"`
	OmitNonNoteTagPrefix bool `default:"true"`
	Database             string
}

type note struct {
	// These come from SQLite
	PK                    int     `db:"Z_PK"`
	ID                    string  `db:"ZUNIQUEIDENTIFIER"`
	Title                 string  `db:"ZTITLE"`
	BodyRaw               []byte  `db:"ZTEXT"`
	CreationTimestamp     float64 `db:"ZCREATIONDATE"`
	ModificationTimestamp float64 `db:"ZMODIFICATIONDATE"`

	// These we parse/produce from ^
	// TODO: What to do with ModificationTimestamp?
	Body              string
	Date              string
	Hashtags          []string
	CustomFrontMatter []string
	Categories        bool
	Tags              bool
	Draft             bool
}

const templateRaw = `---
title: "{{ .Title }}"
date: {{ .Date }}
{{- if .Categories }}
categories: [
{{- range $i, $c := .Hashtags -}}
	{{- if $i -}},{{- end -}}
	"{{- $c -}}"
{{- end -}}
]
{{- end }}
{{- if .Tags }}
tags: [
{{- range $i, $c := .Hashtags -}}
	{{- if $i -}},{{- end -}}
	"{{- $c -}}"
{{- end -}}
]
{{- end }}
draft: {{ .Draft }}
{{- range $l := .CustomFrontMatter }}
{{ $l }}
{{- end }}
---
{{ .Body }}`

// Front matter that Bhugo manages.
var bhugoFrontMatter = map[string]bool{
	"title":      true,
	"date":       true,
	"categories": true,
	"tags":       true,
	"draft":      true,
}

func run(
	_ context.Context,
	args []string) error {
	log.Info("Bhugo Initializing")

	err := godotenv.Load(".bhugo")
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet(args[0], flag.ExitOnError)
	once := flags.Bool("once", false, "Run conversion only once (useful when scripting)")

	debug := flags.Bool("debug", false, "Run with debug level logging")
	err = flags.Parse(args[1:])
	if err != nil {
		return err
	}

	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	var cfg config

	err = envconfig.Process("", &cfg)
	if err != nil {
		return err
	}

	// Override these defaults with the configuration values.
	bhugoFrontMatter["categories"] = cfg.Categories
	bhugoFrontMatter["tags"] = cfg.Tags

	if len(cfg.Database) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		cfg.Database = home + "/Library/Group Containers/9K33E3U3T4.net.shinyfrog.bear/Application Data/database.sqlite"
	}

	db, err := sql.Connect("sqlite3", cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := template.New("Note Template").Parse(templateRaw)
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 2)
	notes := make(chan note, 1)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	wg := sync.WaitGroup{}

	wg.Add(1)
	go updateHugo(db, &wg, done, notes, &cfg, tmpl)

	if *once {
		cache := make(map[string][]byte)
		checkBearOnce(db, notes, cfg.NoteTag, cache)
		done <- true
	} else {
		log.Infof("Watching Bear tag #%s for changes", cfg.NoteTag)

		wg.Add(1)
		go checkBear(&wg, done, db, cfg.Interval, notes, cfg.NoteTag)
	}
	go func() {
		sig := <-sigs
		log.Info(sig)
		done <- true
		done <- true
	}()

	wg.Wait()
	log.Info("Bhugo Exiting")
	return nil
}

func main() {
	ctx := context.Background()
	err := run(ctx, os.Args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}
}

func checkBearOnce(db *sql.DB, notesChan chan<- note, noteTag string, cache map[string][]byte) {
	notes := make([]note, 0, len(cache))
	q := fmt.Sprintf("SELECT Z_PK, ZUNIQUEIDENTIFIER, ZTITLE, ZTEXT, ZCREATIONDATE, ZMODIFICATIONDATE FROM ZSFNOTE WHERE ZTEXT LIKE '%%#%s%%'", noteTag)
	if err := db.Select(&notes, q); err != nil {
		log.Error(err)
		return
	}
	for _, n := range notes {
		c, ok := cache[n.Title]
		switch {
		case !ok:
			log.Infof("Not cached note %s - possibly Hugo", n.Title)
		case bytes.Equal(c, n.BodyRaw):
			continue
		default:
			log.Infof("Differences detected in %s - updating Hugo", n.Title)
		}
		cache[n.Title] = n.BodyRaw
		notesChan <- n
	}
}

func checkBear(wg *sync.WaitGroup, done <-chan bool, db *sql.DB, interval time.Duration, notesChan chan<- note, noteTag string) {
	log.Debug("Starting CheckBear")

	defer wg.Done()

	tick := time.Tick(interval)
	cache := make(map[string][]byte)

	for {
		select {
		case <-tick:
			checkBearOnce(db, notesChan, noteTag, cache)

		case <-done:
			log.Info("Check Bear exiting")
			return
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		log.Fatal(err)
	}
	return true
}

func copyFile(src, dst string) {
	// TODO: Do we care about permissions? Probably not
	srcdata, err := ioutil.ReadFile(src)
	if err != nil {
		log.Error(err)
		return
	}
	if fileExists(dst) {
		dstdata, err := ioutil.ReadFile(src)
		if err != nil {
			log.Error(err)
			return
		}
		if bytes.Equal(srcdata, dstdata) {
			return
		}
	}
	log.Infof("Copying %s to %s", src, dst)
	err = ioutil.WriteFile(dst, srcdata, 0644)
	if err != nil {
		log.Error(err)
	}
}

func copyImagesToHugo(db *sql.DB, cfg *config, n *note, hugoPath string) {
	if db == nil {
		// unit test
		return
	}
	bearDir := path.Dir(path.Dir(cfg.Database))
	bearImagesDir := bearDir + "/Application Data/Local Files/Note Images"
	rows, err := db.Query("SELECT ZUNIQUEIDENTIFIER,ZFILENAME FROM ZSFNOTEFILE WHERE ZNOTE=?", n.PK)
	if err != nil {
		log.Panic(err)
		return
	}
	for rows.Next() {
		var id, filename string
		err = rows.Scan(&id, &filename)
		if err != nil {
			log.Panic(err)
			return
		}
		bearPath := fmt.Sprintf("%s/%s/%s", bearImagesDir, id, filename)
		copyFile(bearPath, fmt.Sprintf("%s/%s", hugoPath, filename))
	}
}

func updateHugoNote(db *sql.DB, cfg *config, tmpl *template.Template, n *note) error {
	hashTagline := cfg.TagLine
	currentTagline := hashTagline

	log.Debugf("Handling %s", n.Title)
	// Replace smart quotes with regular quotes.
	n.BodyRaw = bytes.ReplaceAll(n.BodyRaw, []byte("“"), []byte("\""))
	n.BodyRaw = bytes.ReplaceAll(n.BodyRaw, []byte("”"), []byte("\""))
	// Jan 1 2001
	coreDataEpochOffset := int64(978307200)

	n.Date = time.Unix(int64(n.CreationTimestamp)+coreDataEpochOffset, 0).Format(cfg.TimeFormat)

	lines := bytes.Split(n.BodyRaw, []byte("\n"))

	if hashTagline < 0 {
		// Remove the empty lines from the end
		last := len(lines) - 1
		for last > 0 && len(lines[last]) == 0 {
			last--
		}
		lines = lines[0 : last+1]

		currentTagline = len(lines) + hashTagline
		if currentTagline < 0 || currentTagline >= len(lines) {
			return nil
		}
	}

	n.Hashtags = scanTags(lines[currentTagline], cfg.NoteTag, cfg.OmitNonNoteTagPrefix)
	for _, c := range n.Hashtags {
		if strings.Contains(strings.ToLower(c), "draft") {
			n.Draft = true
		}
	}

	// Remove the tags
	lines = slices.Delete(lines, currentTagline, currentTagline+1)

	// The Bear hashtags will populate either categories or tags (or both) depending on these bools.
	n.Categories = cfg.Categories
	n.Tags = cfg.Tags

	target := strings.ReplaceAll(strings.ToLower(n.Title), " ", "-")
	// Title is the first line
	n.Body = string(bytes.Join(lines[1:], []byte("\n")))

	postDir := fmt.Sprintf("%s/%s/%s", cfg.HugoDir, cfg.ContentDir, target)
	if err := os.MkdirAll(postDir, os.ModePerm); err != nil {
		return err
	}

	copyImagesToHugo(db, cfg, n, postDir)
	fp := postDir + "/index.md"
	cf, err := ioutil.ReadFile(fp)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// If the file exists, check for any custom front matter to preserve it.
	if len(cf) > 0 {
		n.CustomFrontMatter = customFrontMatter(cf)
	}

	fpTemp := fp + ".tmp"

	f, err := os.Create(fpTemp)
	if err != nil {
		return err
	}

	if err := tmpl.Execute(f, n); err != nil {
		log.Error(err)
	}

	if err := f.Close(); err != nil {
		log.Error(err)
	}
	if existed {
		cf, _ := ioutil.ReadFile(fp)
		cf2, _ := ioutil.ReadFile(fpTemp)
		if bytes.Equal(cf, cf2) {
			log.Info("Files are same, skipping update")
			os.Remove(fpTemp)
			return nil
		}
		log.Info("Files differed, updating")
	} else {
		log.Info("Files did not exist, updating")
	}
	return os.Rename(fpTemp, fp)
}

func updateHugo(db *sql.DB, wg *sync.WaitGroup, done <-chan bool, notes <-chan note, cfg *config, tmpl *template.Template) {
	log.Debug("Starting UpdateHugo")
	defer wg.Done()

	for {
		select {
		case n := <-notes:
			err := updateHugoNote(db, cfg, tmpl, &n)
			if err != nil {
				log.Error(err)
			}
		default:
			// we want to empty the notes channel and only
			// then consider done; this facilitates easier
			// use of this elsewhere (if we really cannot
			// process all entries, there are bigger
			// problems)
			select {
			case n := <-notes:
				err := updateHugoNote(db, cfg, tmpl, &n)
				if err != nil {
					log.Error(err)
				}
			case <-done:
				log.Debug("Update Hugo exiting")
				return
			}
		}
	}
}

func scanTags(l []byte, tag string, omitOthers bool) []string {
	start := 0
	end := 0
	inHash := false
	multiWord := false
	hashtags := []string{}
	var prev rune

	for i, r := range l {
		var peek rune
		if i < (len(l) - 1) {
			peek = rune(l[i+1])
		} else {
			peek = 0
		}

		switch {
		// When a starting hashtag is found, initialize the starting point index.
		case r == '#' && (prev == ' ' || prev == 0) && !inHash:
			start = i + 1
			inHash = true
			end = start

		// When the previous character isn't a space and the current is a hash,
		// then this must be the end of a multi-word hash.
		case prev != ' ' && r == '#':
			end = i

		// If currently scanning a hash and a space is found without a subsequent
		// hash then this is either a multi-word hash or some unrelated text
		// so store the current position as the possible end of the hash.
		case inHash && r == ' ' && peek != '#':
			end = i
			multiWord = true

		// When a space is found followed by a hash, then this must
		// be the end of the current hash.
		case r == ' ' && peek == '#' && inHash:
			inHash = false
			multiWord = false

			if !omitOthers || bytes.Equal(l[start:start+len(tag)],
				[]byte(tag)) {
				hashtags = append(hashtags, formatTag(l[start:end], tag))
			}

		// If this isn't a potential multi-word hash, then keep incrementing the end index.
		case !multiWord:
			end = i + 1
		}

		prev = rune(r)
	}

	if inHash {
		if !omitOthers || bytes.Equal(l[start:start+len(tag)],
			[]byte(tag)) {
			hashtags = append(hashtags, formatTag(l[start:end], tag))
		}
	}

	return hashtags
}

func customFrontMatter(f []byte) []string {
	lines := bytes.Split(f, []byte("\n"))
	fm := []string{}

	for i, l := range lines {
		kv := bytes.Split(l, []byte(":"))

		switch {
		case i == 0:
			// First line should be dashes.
			if !bytes.Equal(l, []byte("---")) {
				return []string{}
			}

		// If this line is front matter that Bhugo controls, don't append it.
		case bhugoFrontMatter[string(kv[0])]:
			continue
		case bytes.Equal(l, []byte("---")):
			return fm
		default:
			fm = append(fm, string(l))
		}
	}

	// Should not reach this if file is formatted correctly.
	return []string{}
}

var titleCaser = cases.Title(language.English)

func formatTag(l []byte, tag string) string {
	realtag := strings.TrimSuffix(
		strings.TrimSpace(string(l)), "#")
	return titleCaser.String(strings.TrimPrefix(realtag, tag+"/"))
}
