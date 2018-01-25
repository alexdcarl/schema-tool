// <--
// Copyright © 2017 AppNexus Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// -->

package chain

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/appnexus/schema-tool/lib/log"
)

// Direction of either Up or Down that alter can represent, defined below
type Direction int

const (
	// Undefined direction is used as a placeholder or as an error when
	// parsing directions from alters.
	Undefined Direction = iota

	// Up direction represents an alter that progresses a schema forward
	// and represents new change.
	Up

	// Down direction represents the undoing of an Up alter and is a change
	// that represents negative progress.
	Down
)

// Alter represents a single file within a chain along with the meta-data
// parsed from the file's headers.
type Alter struct {
	FileName  string
	Direction Direction

	// Internal temporary values used to build the chain
	ref     string
	backRef string

	// skipped and required environments. Not exported at the alter-level
	// because validations must be completed at the AlterGroup level and
	// the information is duplicated there.
	requireEnv []string
	skipEnv    []string
}

func newDefaultAlter() *Alter {
	return &Alter{
		Direction:  Undefined,
		requireEnv: make([]string, 0, 4),
		skipEnv:    make([]string, 0, 4),
	}
}

// AlterGroup represents and up/down pair of Alter objects along with links to
// "forward" (child) and "back" (parent) AlterGroup objects.
//
// AlterGroup objects are a node in a doubly-linked list
type AlterGroup struct {
	Up         *Alter
	Down       *Alter
	ForwardRef *AlterGroup
	BackRef    *AlterGroup
	RequireEnv []string
	SkipEnv    []string
}

// Chain is a container to point to the head and tail of a linked list of
// AlterGroup objects.
type Chain struct {
	Head *AlterGroup
	Tail *AlterGroup
}

const (
	// ErrCyclicChain indicates a cyclic chain (no head or tail)
	ErrCyclicChain = iota
	// ErrDuplicateRef indicates a repeated unique 'ref' identifier across unique alters
	ErrDuplicateRef = iota
	// ErrInvalidMetaData represents the majority of errors encountered when parsing
	// meta-data of an alter.
	ErrInvalidMetaData = iota
	// ErrMissingAlterPair indicates an up without a down alter (or vice versa)
	ErrMissingAlterPair = iota
	// ErrNonexistentDirectory indicates that a directory cannot be scanned nor an alter-
	// chain created because the given directory cannot be found or is not a directory
	ErrNonexistentDirectory int = iota
	// ErrUnreadableAlter indicates that a file believed to be an alter file is unreadable
	// for some reason outside the scope of this program
	ErrUnreadableAlter = iota
	// ErrEmptyDirectory indicates that the directory scanned contains no schema files
	ErrEmptyDirectory = iota
)

// Error is a custom error type that implements the error interface but
// carriers some extra context as to the cause of the error (to be used programatically).
type Error struct {
	ErrType    int
	Message    string
	Underlying error
}

func (ce *Error) Error() string {
	return ce.Message
}

// BuildAndValidateChain takes a set of AlterGroups generated from a schema
// directory, applies some extra validations, builds, and returns a chain
// (linked list of type `Chain`) of alters.
func BuildAndValidateChain(groups map[string]*AlterGroup) (*Chain, *Error) {

	for _, group := range groups {
		// Validate groups have up/down pair
		if group.Up == nil || group.Down == nil {
			missingDirection := "down"
			other := group.Up
			if group.Up == nil {
				missingDirection = "up"
				other = group.Down
			}
			return nil, &Error{
				ErrType: ErrMissingAlterPair,
				Message: fmt.Sprintf("Missing %s alter for '%s'", missingDirection, other.ref),
			}
		}

		// validate matching back-ref's
		if group.Up.backRef != group.Down.backRef {
			return nil, &Error{
				ErrType: ErrInvalidMetaData,
				Message: fmt.Sprintf("'back-ref' values for %s do not match (%s and %s)",
					group.Up.ref, group.Up.backRef, group.Down.backRef),
			}
		}

		// Validate skip-env(s) for group
		if len(group.Up.skipEnv) != len(group.Down.skipEnv) {
			return nil, &Error{
				ErrType: ErrInvalidMetaData,
				Message: fmt.Sprintf(
					"Different number of skip-env's found in:\n"+
						"\t%s\n\t%s\n"+
						"These files must contain the same skip-env values.",
					group.Up.FileName, group.Down.FileName),
			}
		}
		for _, skipUp := range group.Up.skipEnv {
			found := false
			for _, skipDown := range group.Down.skipEnv {
				if skipUp == skipDown {
					found = true
					break
				}
			}
			if !found {
				return nil, &Error{
					ErrType: ErrInvalidMetaData,
					Message: fmt.Sprintf(
						"skip-env value '%s' is not found in both up & down alters", skipUp),
				}
			}
		}
		group.SkipEnv = group.Up.skipEnv

		// Validate require-env(s) for group
		if len(group.Up.requireEnv) != len(group.Down.requireEnv) {
			return nil, &Error{
				ErrType: ErrInvalidMetaData,
				Message: fmt.Sprintf("Uneven number of require-env's found in '%s' and '%s'",
					group.Up.FileName, group.Down.FileName),
			}
		}
		for _, requireUp := range group.Up.requireEnv {
			found := false
			for _, requireDown := range group.Down.requireEnv {
				if requireUp == requireDown {
					found = true
					break
				}
			}
			if !found {
				return nil, &Error{
					ErrType: ErrInvalidMetaData,
					Message: fmt.Sprintf(
						"require-env value '%s' is not found in both up & down alters",
						requireUp),
				}
			}
		}
		group.RequireEnv = group.Up.requireEnv
	}

	// Start to build the chain, but while building watch for:
	//   - divergent (split) chains
	//   - backRef's are valid (point to something)

	var head *AlterGroup
	var tail *AlterGroup

	for _, group := range groups {
		backRef := group.Up.backRef
		if backRef == "" {
			// could be a head-alter, skip
			continue
		}
		parent, ok := groups[backRef]
		if !ok {
			return nil, &Error{
				ErrType: ErrInvalidMetaData,
				Message: fmt.Sprintf("Invalid backref '%s' found for '%s'",
					backRef, group.Up.FileName),
			}
		}

		// is always nil before set, impossible for previous loop to write this value
		group.BackRef = parent

		// If a forward-ref is not nil, then it has previously been established as a
		// parent alter. We have found a divergence in the chain.
		if parent.ForwardRef != nil {
			return nil, &Error{
				ErrType: ErrInvalidMetaData,
				Message: fmt.Sprintf(
					"Duplicate parent defined in %s and %s - both point to %s. Chain must be linear.",
					parent.ForwardRef.Up.ref,
					group.Up.ref,
					parent.Up.ref),
			}
		}
		parent.ForwardRef = group
	}

	// Get head & tail from built chain and also make sure that no duplicate roots
	// are found. As for other potential errors:
	//   - abandoned alters
	//   - multiple tails (no next-refs)
	// These are already validated. Abandoned alters would have invalid refs,
	// duplicate parents, or be identified as a duplicate root. Tails would be
	// directed earlier as a divergent chain.
	for _, group := range groups {
		if group.BackRef == nil {
			if head != nil {
				return nil, &Error{
					ErrType: ErrInvalidMetaData,
					Message: fmt.Sprintf(
						"Duplicate root alters found (%s and %s). Chain must have one root alter.",
						group.Up.ref,
						head.Up.ref),
				}
			}
			head = group
		}
		// Cannot have duplicate tail without already encountering another error
		if group.ForwardRef == nil {
			tail = group
		}
	}

	if head == nil || tail == nil {
		return nil, &Error{
			ErrType: ErrCyclicChain,
			Message: "Chain is cyclic and has no head or tail",
		}
	}

	chain := &Chain{
		Head: head,
		Tail: tail,
	}
	return chain, nil
}

// ScanDirectory scans a given directory and return a mapping of AlterRef to
// AlterGroup objects. The objects returned are un-validated aside from
// meta-data parsing.
func ScanDirectory(dir string) (map[string]*AlterGroup, *Error) {
	stat, err := os.Stat(dir)
	if err != nil || !stat.IsDir() {
		return nil, &Error{
			Underlying: err,
			Message:    fmt.Sprintf("Path '%s' is not a directory", dir),
			ErrType:    ErrNonexistentDirectory,
		}
	}

	alters := make(map[string]*AlterGroup)
	files, err := ioutil.ReadDir(dir)
	for _, f := range files {
		if f.IsDir() {
			// only process top-level of dir
			continue
		}
		if isAlterFile(f.Name()) {
			filePath := path.Join(dir, f.Name())

			header, cErr := readHeader(dir + "/" + f.Name())
			if cErr != nil {
				return nil, cErr
			}

			alter, cErr := parseMeta(header, filePath)
			if cErr != nil {
				return nil, cErr
			}
			group, ok := alters[alter.ref]
			if !ok {
				group = &AlterGroup{}
			}
			if alter.Direction == Up {
				if group.Up != nil {
					return nil, &Error{
						ErrType: ErrDuplicateRef,
						Message: fmt.Sprintf("Duplicate 'up' alter for ref '%s'", alter.ref),
					}
				}
				group.Up = alter
			} else if alter.Direction == Down {
				if group.Down != nil {
					return nil, &Error{
						ErrType: ErrDuplicateRef,
						Message: fmt.Sprintf("Duplicate 'down' alter for ref '%s'", alter.ref),
					}
				}
				group.Down = alter
			}
			alters[alter.ref] = group
		}
	}

	if len(alters) == 0 {
		return nil, &Error{
			ErrType: ErrEmptyDirectory,
			Message: fmt.Sprintf("Directory '%s' does not contain any alters", dir),
		}
	}

	return alters, nil
}

// Check if the file is an "alter" by seeing if the name confirms to
// what we expect.
func isAlterFile(name string) bool {
	var filenameRegex = regexp.MustCompile(`^(\d+)-([^-]+-)+(up|down).sql$`)
	return filenameRegex.MatchString(name)
}

// Read the first N lines of an alter file that represent the "header." This is
// the bit of stuff that contains all the meta-data required in alters.
func readHeader(filePath string) ([]string, *Error) {
	var headerRegex = regexp.MustCompile(`^--`)
	lines := make([]string, 256)

	file, err := os.Open(filePath)
	if err != nil {
		return lines, &Error{
			ErrType:    ErrUnreadableAlter,
			Message:    fmt.Sprintf("Unable to read file '%s'", filePath),
			Underlying: err,
		}
	}
	// clone file after we return
	defer file.Close()

	// read line by line
	scanner := bufio.NewScanner(file)
	i := 0
	for scanner.Scan() {
		if i == 256 {
			return lines, &Error{
				ErrType: ErrInvalidMetaData,
				Message: `Header lines (continuous block of lines starting with '--')
exceeds 256. Please add a blank line in-between the meta-data and any
comment lines that may follow.`,
			}
		}
		line := scanner.Text()
		if headerRegex.MatchString(line) {
			lines[i] = line
			i++
		} else {
			// hit non-header line, we're done
			return lines, nil
		}
	}

	if err = scanner.Err(); err != nil {
		return lines, &Error{
			ErrType:    ErrUnreadableAlter,
			Message:    fmt.Sprintf("Unable to read file '%s'", filePath),
			Underlying: err,
		}
	}

	return lines, nil
}

// Parse the meta-information from the file and return an Alter object.
// Returns error if meta cannot be obtained or required information is
// missing.
func parseMeta(lines []string, filePath string) (*Alter, *Error) {
	// expect meta-lines to be single-line and in the form of
	//   "-- key: value"
	// regex checks for extraneous whitespace
	var metaEntryRegex = regexp.MustCompile(`^--\s*([^\s]+)\s*:(.+)\s*$`)

	var alter = newDefaultAlter()
	alter.FileName = filePath

	for _, line := range lines {
		if matches := metaEntryRegex.FindStringSubmatch(line); len(matches) == 3 {
			// 3 matches means we're good to go
			key := strings.ToLower(strings.TrimSpace(matches[1]))
			value := strings.TrimSpace(matches[2])

			switch key {
			case "ref":
				if !isValidRef(value) {
					return nil, &Error{
						ErrType: ErrInvalidMetaData,
						Message: "Invalid 'ref' value found in " + filePath,
					}
				}
				alter.ref = value
			case "backref":
				if value == "" {
					return nil, &Error{
						ErrType: ErrInvalidMetaData,
						Message: fmt.Sprintf("Invalid 'backref' value found in '%s'", filePath),
					}
				}
				alter.backRef = value
			case "direction":
				valueLower := strings.ToLower(value)
				if valueLower == "up" {
					alter.Direction = Up
				} else if valueLower == "down" {
					alter.Direction = Down
				} else {
					return nil, &Error{
						ErrType: ErrInvalidMetaData,
						Message: fmt.Sprintf("Invalid direction '%s' found in '%s'",
							valueLower, filePath),
					}
				}
			case "require-env":
				requiredEnvs := strings.Split(value, ",")
				for _, env := range requiredEnvs {
					trimmedStr := strings.TrimSpace(env)
					if trimmedStr != "" {
						alter.requireEnv = append(alter.requireEnv, trimmedStr)
					}
				}
			case "skip-env":
				skipEnvs := strings.Split(value, ",")
				for _, env := range skipEnvs {
					trimmedStr := strings.TrimSpace(env)
					if trimmedStr != "" {
						alter.skipEnv = append(alter.skipEnv, trimmedStr)
					}
				}
			default:
				log.Warn.Printf("Unknown property '%s' found in '%s'\n", key, filePath)
			}
		}
	}

	if alter.ref == "" {
		return nil, &Error{
			ErrType: ErrInvalidMetaData,
			Message: "Missing required field 'ref'",
		}
	}
	// Note: backref isn't necessary here cause it could be the init file
	if alter.Direction == Undefined {
		return nil, &Error{
			ErrType: ErrInvalidMetaData,
			Message: "Missing required field 'direction'",
		}
	}
	if len(alter.requireEnv) > 0 && len(alter.skipEnv) > 0 {
		return nil, &Error{
			ErrType: ErrInvalidMetaData,
			Message: "Mutually exclusive fields 'require-env' and 'skip-env' cannot be used together",
		}
	}

	return alter, nil
}

// Validate that the ref is a valid identifier
func isValidRef(ref string) bool {
	var refRegex = regexp.MustCompile(`^[\da-zA-Z]+$`)
	return refRegex.MatchString(ref)
}
