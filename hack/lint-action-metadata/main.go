// Command lint-action-metadata rejects "${{ }}" expressions in composite
// action.yml fields that GitHub evaluates at metadata-load time (before any
// step runs) rather than at step-execution time. GitHub's actionlint only
// checks workflow YAML, not action.yml, so this check catches what it
// cannot: a metadata expression referencing an out-of-scope context (e.g.
// "runner" in a description) fails the whole action to load, with no step
// ever executing.
//
// Expressions are only meaningful - and only evaluated in a defined
// context - inside runs.steps[*] (composite step fields: run, env, with,
// if, name) and outputs.*.value (the documented way to surface a step
// output). Everywhere else in action.yml (description, input defaults,
// name, author, branding) a "${{ }}" is always a mistake: it is either
// dead text or, per the incident this check exists for, fatal at load.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type violation struct {
	file string
	line int
	path string
	text string
}

func main() {
	matches, err := filepath.Glob(".github/actions/*/action.yml")
	if err != nil {
		fmt.Fprintln(os.Stderr, "lint-action-metadata:", err)
		os.Exit(2)
	}
	if len(matches) == 0 {
		fmt.Fprintln(os.Stderr, "lint-action-metadata: no .github/actions/*/action.yml found")
		os.Exit(2)
	}

	var violations []violation
	for _, path := range matches {
		vs, err := lintFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lint-action-metadata: %s: %v\n", path, err)
			os.Exit(2)
		}
		violations = append(violations, vs...)
	}

	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "%s:%d: %s: expression not evaluated in a defined context here: %s\n",
				v.file, v.line, v.path, v.text)
		}
		fmt.Fprintf(os.Stderr, "\nlint-action-metadata: %d violation(s): \"${{ }}\" is only valid"+
			" inside runs.steps[*] or outputs.*.value\n", len(violations))
		os.Exit(1)
	}
}

func lintFile(path string) ([]violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}

	var violations []violation
	walk(path, doc.Content[0], nil, false, &violations)
	return violations, nil
}

// walk descends the action.yml mapping tree. allowed marks a subtree where
// "${{ }}" is meaningful; path records the key chain for error messages.
func walk(file string, node *yaml.Node, path []string, allowed bool, out *[]violation) {
	switch node.Kind {
	case yaml.DocumentNode:
		for _, c := range node.Content {
			walk(file, c, path, allowed, out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			val := node.Content[i+1]
			childPath := append(append([]string{}, path...), key.Value)
			childAllowed := allowed || allowedSubtree(childPath)
			walk(file, val, childPath, childAllowed, out)
		}
	case yaml.SequenceNode:
		for _, c := range node.Content {
			walk(file, c, path, allowed, out)
		}
	case yaml.ScalarNode:
		if !allowed && strings.Contains(node.Value, "${{") {
			*out = append(*out, violation{
				file: file,
				line: node.Line,
				path: strings.Join(path, "."),
				text: node.Value,
			})
		}
	}
}

// allowedSubtree reports whether GitHub evaluates "${{ }}" for everything
// under the given key path: composite steps, and a declared output's value.
func allowedSubtree(path []string) bool {
	if len(path) >= 2 && path[0] == "runs" && path[1] == "steps" {
		return true
	}
	// outputs.<id>.value
	if len(path) == 3 && path[0] == "outputs" && path[2] == "value" {
		return true
	}
	return false
}
