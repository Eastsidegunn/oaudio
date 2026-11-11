package main

import (
	"bufio"
	"os"
	"strings"
	"unicode"
)

// lexicon is the set of words the engine can pronounce.
//
// sherpa-onnx v2 pronounces Kokoro text by dictionary lookup only and
// silently skips any word it does not find, so a name such as "Oaudio" would
// simply not be spoken. Dropping a word is worse than approximating it, so
// unknown words are respelled as a sequence of known ones ("o voice"), or as
// letters when no such split exists.
type lexicon struct {
	words map[string]bool
}

func loadLexicon(path string) (*lexicon, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l := &lexicon{words: map[string]bool{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if word, _, ok := strings.Cut(sc.Text(), " "); ok && word != "" {
			l.words[strings.ToLower(word)] = true
		}
	}
	return l, sc.Err()
}

// respell rewrites the unknown words of text. Known words, punctuation,
// digits and non-Latin text pass through untouched.
func (l *lexicon) respell(text string) string {
	var out strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); {
		if !isWordRune(runes[i]) {
			out.WriteRune(runes[i])
			i++
			continue
		}
		j := i
		for j < len(runes) && (isWordRune(runes[j]) || (runes[j] == '\'' && j+1 < len(runes) && isWordRune(runes[j+1]))) {
			j++
		}
		word := string(runes[i:j])
		if l.words[strings.ToLower(word)] {
			out.WriteString(word)
		} else {
			out.WriteString(strings.Join(l.split(strings.ToLower(strings.ReplaceAll(word, "'", ""))), " "))
		}
		i = j
	}
	return out.String()
}

func isWordRune(r rune) bool {
	return r <= unicode.MaxASCII && unicode.IsLetter(r)
}

// split breaks an unknown word into the fewest known pieces. Pieces shorter
// than three letters are avoided unless nothing else works, because short
// dictionary entries ("a", "to", "ed") rarely sound like the letters they
// replace; a word that cannot be split is spelled out.
func (l *lexicon) split(word string) []string {
	n := len(word)
	const inf = 1 << 30
	cost := make([]int, n+1)
	prev := make([]int, n+1)
	for i := 1; i <= n; i++ {
		cost[i] = inf
		for j := 0; j < i; j++ {
			if cost[j] == inf || !l.words[word[j:i]] {
				continue
			}
			c := cost[j] + 10
			if i-j < 3 {
				c += 25 // strongly prefer longer pieces
			}
			if c < cost[i] {
				cost[i], prev[i] = c, j
			}
		}
	}
	if cost[n] >= inf {
		return strings.Split(word, "") // spell it
	}
	var pieces []string
	for i := n; i > 0; i = prev[i] {
		pieces = append([]string{word[prev[i]:i]}, pieces...)
	}
	// Many tiny pieces sound no better than spelling and take longer to say.
	if len(pieces) > n/3+1 {
		return strings.Split(word, "")
	}
	return pieces
}
