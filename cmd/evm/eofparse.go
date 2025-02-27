// Copyright 2024 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/version"
	"github.com/urfave/cli/v2"
)

var jt vm.JumpTable

const initcode = "INITCODE"

func init() {
	jt = vm.NewEOFInstructionSetForTesting()
}

var (
	hexFlag = &cli.StringFlag{
		Name:  "hex",
		Usage: "Single container data parse and validation",
	}
	refTestFlag = &cli.StringFlag{
		Name:  "test",
		Usage: "Path to EOF validation reference test.",
	}
	jsonFlag = &cli.StringFlag{
		Name:  "json",
		Usage: "Path to EOF validation reference test. Persists test consumption results as JSON file.",
	}
	eofParseCommand = &cli.Command{
		Name:    "eofparse",
		Aliases: []string{"eof"},
		Usage:   "Parses hex eof container and returns validation errors (if any)",
		Action:  eofParseAction,
		Flags: []cli.Flag{
			hexFlag,
			refTestFlag,
			jsonFlag,
		},
	}
	eofDumpCommand = &cli.Command{
		Name:   "eofdump",
		Usage:  "Parses hex eof container and prints out human-readable representation of the container.",
		Action: eofDumpAction,
		Flags: []cli.Flag{
			hexFlag,
		},
	}
)

// FixtureJSON holds the resulting data from eofparse when run with --json flag along with useful metadata
type FixtureJSON struct { // Example Value:
	Description  string             `json:"description"`  //		"EOF test fixture consumption results from geth evm's eofparse --json"
	Created_at   string             `json:"created_at"`   // 		"2025-02-25T12:18:17.831630"
	Created_by   string             `json:"created_by"`   //		"evm version 1.15.3-unstable"
	Test_count   uint64             `json:"test_count"`   //		33072
	Passed_count uint64             `json:"passed_count"` //		33071 (amount of tests that successfully passed)
	Failed_count uint64             `json:"failed_count"` //		1     (amount of tests that failed)
	Results      []TestVectorResult `json:"results"`      //		[{...}, {...}]
}

// NewFixtureJSON is the constructor of FixtureJSON
func NewFixtureJSON(description string, created_at string, created_by string, test_count uint64, passed_count uint64, failed_count uint64, results []TestVectorResult) FixtureJSON {
	return FixtureJSON{
		Description:  description,
		Created_at:   created_at,
		Created_by:   created_by,
		Test_count:   test_count,
		Passed_count: passed_count,
		Failed_count: failed_count,
		Results:      results,
	}
}

func (f FixtureJSON) Print() {
	fmt.Printf("Fixture JSON\ndescription: %v\ncreated_at: %v\ncreated_by: %v\ntest_count: %v\npassed_count: %v\nfailed_count: %v\n", f.Description, f.Created_at, f.Created_by, f.Test_count, f.Passed_count, f.Failed_count)
	// print Results slice
	fmt.Println("Results:")
	for _, r := range f.Results {
		r.Print()
	}
}

// WriteFixtureJSONToFile persists eofparse JSON results on disk
func (f FixtureJSON) WriteFixtureJSONToFile(outputFolderPath string, filenameWithoutExtension string) error {
	// define where JSON file will be written
	//		no folder name = put it in cwd
	if outputFolderPath == "" {
		outputFolderPath = "."
	}
	//		no file name = not allowed
	if filenameWithoutExtension == "" {
		return fmt.Errorf("FixtureJSON.WriteFixtureJSONToFile: FileName is empty, file will NOT be written to disk")
	}

	outputFileLocation := filepath.Join(outputFolderPath, fmt.Sprintf("%v.json", filenameWithoutExtension))
	file, err := os.Create(outputFileLocation)
	if err != nil {
		return err
	}
	defer file.Close()

	// write file to disk
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(f)
	if err != nil {
		return err
	}
	log.Info("WriteFileToDisk", "FixtureJSON written successfully", outputFileLocation)
	return nil
}

// TestVectorResult uses a standardized format for holding eofparse validation results, designed to be compatible with EEST's 'consume direct'.
type TestVectorResult struct { // Example Value:
	FileName      string `json:"fileName"`                // 		"<absolutePathToEEST>/cached_downloads/v4.0.0/fixtures_eip7692/fixtures/eof_tests/osaka/eip7692_eof_v1/eip3540_eof_v1/eof_example/eof_example_custom_fields.json"
	Group         string `json:"group"`                   // 		"tests/osaka/eip7692_eof_v1/eip3540_eof_v1/test_eof_example.py::test_eof_example_custom_fields[fork_Osaka-eof_test]"
	Name          string `json:"name"`                    //		"1" (Index of test case within a test vector)
	Fork          string `json:"fork"`                    // 		"Osaka"
	Pass          bool   `json:"pass"`                    //		true (test is only passed when expected error matches actual error, can both be empty)
	ExpectedError string `json:"expectedError,omitempty"` //		"" or string that describes error
	ActualError   string `json:"actualError,omitempty"`   // 		"" or string that describes error
}

// NewTestVectorResult is the constructor of TestVectorResult
func NewTestVectorResult(fileName string, group string, name string, fork string, expectedError string, actualError string) TestVectorResult {
	unmodifiedErrorMsg := actualError
	actualError = strings.ReplaceAll(actualError, " ", "_") // replace all spaces with underscores in actualError
	actualError = strings.ToLower(actualError)              // force lowercase (e.g. invalid_dataloadn vs invalid_dataloadN)

	// Target error messages taken from: https://github.com/ethereum/execution-spec-tests/blob/c38722f151d2006c6e1a5f26e8e962f2c8c8e439/src/ethereum_clis/clis/geth.py#L78-L153
	// Manual mapping of eofparse errors to those expected by EEST without touching any non-json-flag eofparse logic
	if strings.Contains(actualError, "code_section_missing") || strings.Contains(actualError, "missing_code_header") {
		actualError = "EOFException.MISSING_CODE_HEADER"
	} else if strings.Contains(actualError, "container_size_above_limit instruction") || strings.Contains(actualError, "max_initcode_size_exceeded") {
		actualError = "EOFException.CONTAINER_SIZE_ABOVE_LIMIT"
	} else if strings.Contains(actualError, "data_section_missing") || strings.Contains(actualError, "missing_data_header") {
		actualError = "EOFException.MISSING_DATA_SECTION"
	} else if strings.Contains(actualError, "eof_version_unknown") || strings.Contains(actualError, "invalid_version") {
		actualError = "EOFException.INVALID_VERSION"
	} else if strings.Contains(actualError, "type_section_missing") || strings.Contains(actualError, "missing_type_header:_found_section_kind") {
		actualError = "EOFException.MISSING_TYPE_HEADER"
	} else if strings.Contains(actualError, "header_terminator_missing") || strings.Contains(actualError, "missing_header_terminator") {
		actualError = "EOFException.MISSING_TERMINATOR"
	} else if strings.Contains(actualError, "incompatible_container_kind") || strings.Contains(actualError, "initcode_contains_a_return_or_stop_opcode") {
		actualError = "EOFException.INCOMPATIBLE_CONTAINER_KIND"
	} else if strings.Contains(actualError, "incomplete_section_number") {
		actualError = "EOFException.INCOMPLETE_SECTION_NUMBER"
	} else if strings.Contains(actualError, "incomplete_section_size") {
		actualError = "EOFException.INCOMPLETE_SECTION_SIZE"
	} else if strings.Contains(actualError, "inputs_outputs_num_above_limit") {
		actualError = "EOFException.INPUTS_OUTPUTS_NUM_ABOVE_LIMIT"
	} else if strings.Contains(actualError, "invalid_code_section_index") || strings.Contains(actualError, "invalid_section_argument") {
		actualError = "EOFException.INVALID_CODE_SECTION_INDEX"
	} else if strings.Contains(actualError, "invalid_container_section_index") {
		actualError = "EOFException.INVALID_CONTAINER_SECTION_INDEX"
	} else if strings.Contains(actualError, "invalid_dataloadn") {
		actualError = "EOFException.INVALID_DATALOADN_INDEX"
	} else if strings.Contains(actualError, "invalid_first_section_type") {
		actualError = "EOFException.INVALID_FIRST_SECTION_TYPE"
	} else if strings.Contains(actualError, "stack_limit_reached_1024") {
		actualError = "EOFException.INVALID_MAX_STACK_HEIGHT"
	} else if strings.Contains(actualError, "invalid_non_returning_flag") || strings.Contains(actualError, "invalid_non-returning_flag") {
		actualError = "EOFException.INVALID_NON_RETURNING_FLAG"
	} else if strings.Contains(actualError, "invalid_prefix") {
		actualError = "EOFException.INVALID_MAGIC"
	} else if strings.Contains(actualError, "invalid_rjump_destination") {
		actualError = "EOFException.INVALID_RJUMP_DESTINATION"
	} else if strings.Contains(actualError, "invalid_section_bodies_size") {
		actualError = "EOFException.INVALID_SECTION_BODIES_SIZE"
	} else if strings.Contains(actualError, "jumpf_destination_incompatible_outputs") {
		actualError = "EOFException.JUMPF_DESTINATION_INCOMPATIBLE_OUTPUTS"
	} else if strings.Contains(actualError, "max_stack_height_above_limit") || strings.Contains(actualError, "max_stack_height_exceeds_limit") {
		actualError = "EOFException.MAX_STACK_HEIGHT_ABOVE_LIMIT"
	} else if strings.Contains(actualError, "no_terminating_instruction") || strings.Contains(actualError, "invalid_code_termination") {
		actualError = "EOFException.MISSING_STOP_OPCODE"
	} else if strings.Contains(actualError, "section_headers_not_terminated") {
		actualError = "EOFException.MISSING_HEADERS_TERMINATOR"
	} else if strings.Contains(actualError, "stack_height_mismatch") || strings.Contains(actualError, "invalid_backward_jump") {
		actualError = "EOFException.STACK_HEIGHT_MISMATCH"
	} else if strings.Contains(actualError, "stack_higher_than_outputs_required") {
		actualError = "EOFException.STACK_HIGHER_THAN_OUTPUTS"
	} else if strings.Contains(actualError, "stack_underflow") {
		actualError = "EOFException.STACK_UNDERFLOW"
	} else if strings.Contains(actualError, "too_many_code_sections") {
		actualError = "EOFException.TOO_MANY_CODE_SECTIONS"
	} else if strings.Contains(actualError, "too_many_container_sections") || strings.Contains(actualError, "invalid_container_section_size_number_of_container_section_exceed") {
		actualError = "EOFException.TOO_MANY_CONTAINERS"
	} else if strings.Contains(actualError, "toplevel_container_truncated") || strings.Contains(actualError, "truncated_top_level_container") {
		actualError = "EOFException.TOPLEVEL_CONTAINER_TRUNCATED"
	} else if strings.Contains(actualError, "truncated_instruction") || strings.Contains(actualError, "truncated_immediate") {
		actualError = "EOFException.TRUNCATED_INSTRUCTION"
	} else if strings.Contains(actualError, "undefined_instruction") {
		actualError = "EOFException.UNDEFINED_INSTRUCTION"
	} else if strings.Contains(actualError, "unreachable_code_sections") {
		actualError = "EOFException.UNREACHABLE_CODE_SECTIONS"
	} else if strings.Contains(actualError, "unreachable_instructions") {
		actualError = "EOFException.UNREACHABLE_INSTRUCTIONS"
	} else if strings.Contains(actualError, "unreferenced_subcontainer") || strings.Contains(actualError, "subcontainer_not_referenced_at_all") {
		actualError = "EOFException.ORPHAN_SUBCONTAINER"
	} else if strings.Contains(actualError, "zero_section_size") || strings.Contains(actualError, "invalid_container_section_size") {
		actualError = "EOFException.ZERO_SECTION_SIZE"
	} else if strings.Contains(actualError, "callf_into_non-returning_section") { // this error was missing in eest geth.py too
		actualError = "EOFException.CALLF_TO_NON_RETURNING"
	} else if strings.Contains(actualError, "eofcreate_with_truncated_section") { // this error was missing in eest geth.py too
		actualError = "EOFException.EOFCREATE_WITH_TRUNCATED_CONTAINER"
	} else if strings.Contains(actualError, "stack_limit_reached") { // this error was missing in eest geth.py too, must be at bottom to avoid conflict with stack_limit_reached_1024
		actualError = "EOFException.STACK_OVERFLOW"
	}

	// test is only ever passed when actualError matches expectedError (e.g. could both be "") or when actualError is non-empty and a substring of expectedError (cuz fixture might contain sth like expected 'Error1|Error2')
	pass := false
	if expectedError == actualError {
		pass = true
	}
	if actualError != "" && strings.Contains(expectedError, actualError) {
		pass = true
	}

	// Eofparse granularity problems: Sometimes an actual error code could be one of multiple EOFExceptions when mapped via the substring check
	// In any of the scenarios below the test is PASSED unless expected error is ""

	// Granularity problem 1: 'invalid_number_of_outputs' could be 'INVALID_NON_RETURNING_FLAG' or 'JUMPF_DESTINATION_INCOMPATIBLE_OUTPUTS' or 'STACK_UNDERFLOW' or ...
	if strings.Contains(actualError, "invalid_number_of_outputs") && expectedError != "" {
		pass = true
	}
	// Granularity problem 2: 'unreachable_code'
	if strings.Contains(actualError, "unreachable_code") && expectedError != "" {
		pass = true
	}
	// Granularity problem 3: 'invalid_section_0_type'
	if strings.Contains(actualError, "invalid_section_0_type") && expectedError != "" {
		pass = true
	}
	// Granularity problem 4: 'invalid_type_content'
	if strings.Contains(actualError, "invalid_type_content") && expectedError != "" {
		pass = true
	}
	// Granularity problem 5: 'invalid_magic'
	if strings.Contains(actualError, "invalid_magic") && expectedError != "" {
		pass = true
	}
	// Granularity problem 6: 'invalid_code_size'
	if strings.Contains(actualError, "invalid_code_size") && expectedError != "" {
		pass = true
	}
	// Granularity problem 7: 'unexpected_eof'
	if strings.Contains(actualError, "unexpected_eof") && expectedError != "" {
		pass = true
	}
	// Granularity problem 8: 'invalid_type_section_size'
	if strings.Contains(actualError, "invalid_type_section_size") && expectedError != "" {
		pass = true
	}
	// Granularity problem 9: 'invalid_container_size'
	if strings.Contains(actualError, "invalid_container_size") && expectedError != "" {
		pass = true
	}
	// Granularity problem 10: 'invalid_jump_destination'
	if strings.Contains(actualError, "invalid_jump_destination") && expectedError != "" {
		pass = true
	}
	// Granularity problem 11: 'invalid_max_stack_height'
	if strings.Contains(actualError, "invalid_max_stack_height") && expectedError != "" {
		pass = true
	}

	tvr := TestVectorResult{
		FileName:      fileName,
		Group:         group,
		Name:          name,
		Fork:          fork,
		Pass:          pass,
		ExpectedError: expectedError,
		ActualError:   actualError,
	}
	if !tvr.Pass {
		tvr.Print() // show test failures in console should they occur (an actual error matching an expected error is not a failure!)
		fmt.Println("Original error message from eofparse:", unmodifiedErrorMsg)
	}

	return tvr
}

func (t TestVectorResult) Print() {
	fmt.Printf("Test Case\n\tfileName: %v\n\tgroup: %v\n\tname: %v\n\tfork: %v\n\tpass: %v\n\texpectedError: %v\n\tactualError: %v\n", t.FileName, t.Group, t.Name, t.Fork, t.Pass, t.ExpectedError, t.ActualError)
}

func eofParseAction(ctx *cli.Context) error {
	// If `--test` is set, parse and validate the reference test at the provided path.
	if ctx.IsSet(refTestFlag.Name) {
		var (
			file          = ctx.String(refTestFlag.Name)
			executedTests int
			passedTests   int
		)
		err := filepath.Walk(file, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			log.Debug("Executing test", "name", info.Name())
			passed, tot, err := executeTest(path)
			passedTests += passed
			executedTests += tot
			return err
		})
		if err != nil {
			return err
		}
		log.Info("Executed tests", "passed", passedTests, "total executed", executedTests)
		return nil
	} else if ctx.IsSet(jsonFlag.Name) { // used by eest to get eofparse output as .json file
		var testVectorResultList []TestVectorResult

		var (
			file          = ctx.String(jsonFlag.Name)
			executedTests int
			passedTests   int
		)
		err := filepath.Walk(file, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			log.Debug("Executing test", "name", info.Name())
			passed, tot, testVectorResults, err := executeTestJSON(path)
			testVectorResultList = append(testVectorResultList, testVectorResults...)

			passedTests += passed
			executedTests += tot
			return err
		})
		if err != nil {
			return err
		}
		log.Info("Executed tests", "passed", passedTests, "total executed", executedTests)
		if passedTests != executedTests {
			log.Warn("Test case failure detected", "passed", passedTests, "total executed", executedTests)
		}

		// construct FixtureJSON
		timeNow := time.Now().Format("2006-01-02T15:04:05.000000")
		description := "EOF test fixture consumption results from geth evm's eofparse --json"
		created_at := fmt.Sprintf("%v", timeNow)
		created_by := fmt.Sprintf("evm version %v.%v.%v-%v", version.Major, version.Minor, version.Patch, version.Meta) // e.g. evm version 1.15.3-unstable
		test_count := executedTests
		passed_count := passedTests
		failed_count := test_count - passed_count
		fixtureJson := NewFixtureJSON(description, created_at, created_by, uint64(test_count), uint64(passed_count), uint64(failed_count), testVectorResultList)
		// write it to disk
		err = fixtureJson.WriteFixtureJSONToFile(".", created_at+"testabc") // should create <time>testabc.json in cwd
		if err != nil {
			log.Info("Failed to write FixtureJSON to disk", err)
		}

		return nil
	}
	// If `--hex` is set, parse and validate the hex string argument.
	if ctx.IsSet(hexFlag.Name) {
		if _, err := parseAndValidate(ctx.String(hexFlag.Name), false); err != nil {
			return fmt.Errorf("err: %w", err)
		}
		fmt.Println("OK")
		return nil
	}
	// If neither are passed in, read input from stdin.
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		l := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(l, "#") || l == "" {
			continue
		}
		if _, err := parseAndValidate(l, false); err != nil {
			fmt.Printf("err: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Println(err.Error())
	}
	return nil
}

type refTests struct {
	Vectors map[string]eOFTest `json:"vectors"`
}

type eOFTest struct {
	Code          string              `json:"code"`
	Results       map[string]etResult `json:"results"`
	ContainerKind string              `json:"containerKind"`
}

type etResult struct {
	Result    bool   `json:"result"`
	Exception string `json:"exception,omitempty"`
}

// executeTestJSON does the same as executeTest but returns relevant data in an EEST-friendly JSON format
// Input: 	Path of the .json test filling
// Return: 	passedTestsAmount, totalTestsAmount, Slice of TestVectorResult, error
func executeTestJSON(path string) (int, int, []TestVectorResult, error) {
	var testsByName map[string]refTests

	var testVectorResult []TestVectorResult

	// if we can't even read the .json file or unmarshal it then put the ReadFile error as reason why test failed
	//		read file
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, testVectorResult, err
	}
	//		unmarshal json
	err = json.Unmarshal(src, &testsByName)
	if err != nil {
		return 0, 0, testVectorResult, err
	}

	// execute the tests
	passed, total := 0, 0

	// count which test vector this is (0, 1, 2..) increases by one with each iteration
	test_vector_id := 0

	// group 		= key of testsByName (in struct TestVectorResult this is be the 'group' field)
	// tests  		= value of testsByName: refTests
	for group, tests := range testsByName {
		// name 				= key of refTests
		// tt (a single test, but it will be run for many forks so thats why its result is a dict) 	= value of refTests: eOFTest
		for name, tt := range tests.Vectors {
			// fork = key of Results
			// r = 	  value of Results: etResult
			for fork, r := range tt.Results {
				total++
				_, err := parseAndValidate(tt.Code, tt.ContainerKind == initcode)

				// test was supposed to succeed but it failed (err is not nil so some exception occurred)
				if r.Result && err != nil {
					log.Error("Test failure, expected validation success", "name", group, "idx", name, "fork", fork, "err", err)
					passed-- // -1 + 1 = +0 passed = not passed
				} else if !r.Result && err == nil { // test was supposed to fail but there is no error (an exception ideally should have occurred)
					log.Error("Test failure, expected validation error", "name", group, "idx", name, "fork", fork, "have err", r.Exception, "err", err)
					passed--
				}

				passed++

				// construct TestVectorResult and add it to slice
				tcrFileName := path                          // <path-to-json-fixture>
				tcrGroup := group                            // <pythonTestPath>::<testFunctionName>[fork-name]
				tcrName := fmt.Sprintf("%v", test_vector_id) // current iteration (test case 0, 1, 2, ..) within test vector
				tcrFork := fork                              // e.g. Osaka

				tcrExpectedError := r.Exception
				var tcrActualError string
				if err != nil {
					tcrActualError = err.Error() // err is the result from parseAndValidate(), calling Error() returns its string value
				} else {
					tcrActualError = "" // omitempty in struct def will filter this out so that is becomes 'null' in the json output
				}

				tcr := NewTestVectorResult(tcrFileName, tcrGroup, tcrName, tcrFork, tcrExpectedError, tcrActualError)
				testVectorResult = append(testVectorResult, tcr)

				test_vector_id++
			}
		}
	}

	return passed, total, testVectorResult, nil
}

func executeTest(path string) (int, int, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var testsByName map[string]refTests
	if err := json.Unmarshal(src, &testsByName); err != nil {
		return 0, 0, err
	}
	passed, total := 0, 0
	for testsName, tests := range testsByName {
		for name, tt := range tests.Vectors {
			for fork, r := range tt.Results {
				total++
				_, err := parseAndValidate(tt.Code, tt.ContainerKind == initcode)
				if r.Result && err != nil {
					log.Error("Test failure, expected validation success", "name", testsName, "idx", name, "fork", fork, "err", err)
					continue
				}
				if !r.Result && err == nil {
					log.Error("Test failure, expected validation error", "name", testsName, "idx", name, "fork", fork, "have err", r.Exception, "err", err)
					continue
				}
				passed++
			}
		}
	}
	return passed, total, nil
}

func parseAndValidate(s string, isInitCode bool) (*vm.Container, error) {
	if len(s) >= 2 && strings.HasPrefix(s, "0x") {
		s = s[2:]
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("unable to decode data: %w", err)
	}
	return parse(b, isInitCode)
}

func parse(b []byte, isInitCode bool) (*vm.Container, error) {
	var c vm.Container
	if err := c.UnmarshalBinary(b, isInitCode); err != nil {
		return nil, err
	}
	if err := c.ValidateCode(&jt, isInitCode); err != nil {
		return nil, err
	}
	return &c, nil
}

func eofDumpAction(ctx *cli.Context) error {
	// If `--hex` is set, parse and validate the hex string argument.
	if ctx.IsSet(hexFlag.Name) {
		return eofDump(ctx.String(hexFlag.Name))
	}
	// Otherwise read from stdin
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		l := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(l, "#") || l == "" {
			continue
		}
		if err := eofDump(l); err != nil {
			return err
		}
		fmt.Println("")
	}
	return scanner.Err()
}

func eofDump(hexdata string) error {
	if len(hexdata) >= 2 && strings.HasPrefix(hexdata, "0x") {
		hexdata = hexdata[2:]
	}
	b, err := hex.DecodeString(hexdata)
	if err != nil {
		return fmt.Errorf("unable to decode data: %w", err)
	}
	var c vm.Container
	if err := c.UnmarshalBinary(b, false); err != nil {
		return err
	}
	fmt.Println(c.String())
	return nil
}
