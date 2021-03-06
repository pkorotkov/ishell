// Package ishell implements an interactive shell.
package ishell

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/flynn/go-shlex"
	"gopkg.in/readline.v1"
)

var (
	DefaultPrompt     = "> "
	DefaultNextPrompt = "... "
	OnNoHandler       = func(s *Shell) {
		s.Println("No handler for given input is found")
	}
)

// Shell is an interactive cli shell.
type Shell struct {
	functions      map[string]CmdFunc
	generic        CmdFunc
	interrupt      CmdFunc
	interruptCount int
	reader         *shellReader
	writer         io.Writer
	active         bool
	activate       sync.Once // Add this to reduce the burden
	deactivate     sync.Once // while intensive reading active flag.
	ignoreCase     bool
	historyFile    string
}

// New creates a new shell with default settings. Uses standard output and default prompt.
func New() (*Shell, error) {
	rl, err := readline.New(DefaultPrompt)
	if err != nil {
		return nil, fmt.Errorf("shell or operating system not supported")
	}
	shell := &Shell{
		functions: make(map[string]CmdFunc),
		reader: &shellReader{
			scanner:     rl,
			prompt:      DefaultPrompt,
			multiPrompt: DefaultNextPrompt,
			showPrompt:  true,
			buf:         bytes.NewBuffer(nil),
			completer:   readline.NewPrefixCompleter(),
		},
		writer: os.Stdout,
	}
	addDefaultFuncs(shell)
	return shell, nil
}

// Start starts the shell. It reads inputs from standard input and calls registered functions
// accordingly. This function blocks until the shell is stopped.
func (s *Shell) Start() {
	s.activate.Do(func() {
		s.start()
	})
}

func (s *Shell) start() {
	s.active = true
shell:
	for s.active {
		var line []string
		var err error
		read := make(chan struct{})
		go func() {
			line, err = s.read()
			read <- struct{}{}
		}()
		<-read
		// TODO: Make these case more accurate.
		if err == io.EOF {
			fmt.Println("EOF")
			break shell
		}
		if err != nil && err != readline.ErrInterrupt {
			s.Println("Error:", err)
		}
		if err == readline.ErrInterrupt {
			// interrupt received
			err = handleInterrupt(s, line)
		} else {
			// reset interrupt counter
			s.interruptCount = 0
			// normal flow
			if len(line) == 0 {
				// no input line
				continue shell
			}
			err = handleInput(s, line)
		}
		if err != nil {
			switch e := err.(type) {
			case shellError:
				switch e.level {
				case warnLevel:
					s.Println("Warning due to error:", err)
					continue shell
				case stopLevel:
					s.Println("Stopping due to error:", err)
					return
				case exitLevel:
					s.Println("Exiting due to error:", err)
					return
				case panicLevel:
					panic(err)
				}
			case noHandlerError:
				OnNoHandler(s)
			default:
				s.Println("Unqualified error:", err)
				return
			}
		}
	}
}

func handleInput(s *Shell, line []string) error {
	handled, err := s.handleCommand(line)
	if handled || err != nil {
		return err
	}
	// Generic handler
	if s.generic == nil {
		return newNoHandlerError()
	}
	output, err := s.generic(line...)
	if err != nil {
		return err
	}
	if output != "" {
		s.Println(output)
	}
	return nil
}

func handleInterrupt(s *Shell, line []string) error {
	if s.interrupt == nil {
		return errors.New("No interrupt handler")
	}
	output, err := s.interrupt(line...)
	if output != "" {
		s.Println(output)
	}
	return err
}

func (s *Shell) handleCommand(str []string) (bool, error) {
	cmd := str[0]
	if s.ignoreCase {
		cmd = strings.ToLower(cmd)
	}
	if _, ok := s.functions[cmd]; !ok {
		return false, nil
	}
	output, err := s.functions[cmd](str[1:]...)
	if err != nil {
		return true, err
	}
	if output != "" {
		s.Println(output)
	}
	return true, nil
}

// Stop stops the shell. This will stop the shell from auto reading inputs and calling
// registered functions. A stopped shell is only inactive but totally functional.
// Its functions can still be called.
func (s *Shell) Stop() {
	if !s.active {
		return
	}
	s.deactivate.Do(func() {
		s.active = false
		s.reader.scanner.Close()
	})
}

// ReadLine reads a line from standard input.
func (s *Shell) ReadLine() string {
	line, _ := s.readLine()
	return line
}

func (s *Shell) readLine() (line string, err error) {
	consumer := make(chan lineString)
	defer close(consumer)
	go s.reader.readLine(consumer)
	ls := <-consumer
	return ls.line, ls.err
}

func (s *Shell) read() ([]string, error) {
	heredoc := false
	eof := ""
	// heredoc multiline
	lines, err := s.readMultiLinesFunc(func(line string) bool {
		if !heredoc {
			if strings.Contains(line, "<<") {
				s := strings.SplitN(line, "<<", 2)
				if eof = strings.TrimSpace(s[1]); eof != "" {
					heredoc = true
					return true
				}
			}
		} else {
			return line != eof
		}
		return strings.HasSuffix(strings.TrimSpace(line), "\\")
	})
	if heredoc {
		s := strings.SplitN(lines, "<<", 2)
		args, err1 := shlex.Split(s[0])

		arg := strings.TrimSuffix(strings.SplitN(s[1], "\n", 2)[1], eof)
		args = append(args, arg)
		if err1 != nil {
			return args, err1
		}
		return args, err
	}
	lines = strings.Replace(lines, "\\\n", " \n", -1)
	args, err1 := shlex.Split(lines)
	if err1 != nil {
		return args, err1
	}
	return args, err
}

// ReadMultiLinesFunc reads multiple lines from standard input. It passes each read line to
// f and stops reading when f returns false.
func (s *Shell) ReadMultiLinesFunc(f func(string) bool) string {
	lines, _ := s.readMultiLinesFunc(f)
	return lines
}

func (s *Shell) readMultiLinesFunc(f func(string) bool) (string, error) {
	lines := bytes.NewBufferString("")
	currentLine := 0
	var err error
	for {
		if currentLine == 1 {
			// from second line, enable next line prompt.
			s.reader.setMultiMode(true)
		}
		var line string
		line, err = s.readLine()
		fmt.Fprint(lines, line)
		if !f(line) || err != nil {
			break
		}
		fmt.Fprintln(lines)
		currentLine++
	}
	if currentLine > 0 {
		// if more than one line is read
		// revert to standard prompt.
		s.reader.setMultiMode(false)
	}
	return lines.String(), err
}

// ReadMultiLines reads multiple lines from standard input. It stops reading when terminator
// is encountered at the end of the line. It returns the lines read including terminator.
// For more control, use ReadMultiLinesFunc.
func (s *Shell) ReadMultiLines(terminator string) string {
	return s.ReadMultiLinesFunc(func(line string) bool {
		if strings.HasSuffix(strings.TrimSpace(line), terminator) {
			return false
		}
		return true
	})
}

// ReadPassword reads password from standard input without echoing the characters.
// Note that this only works as expected when the standard input is a terminal.
func (s *Shell) ReadPassword() string {
	return s.reader.readPassword()
}

// Println prints to output and ends with newline character.
func (s *Shell) Println(val ...interface{}) {
	s.reader.buf.Truncate(0)
	fmt.Fprintln(s.writer, val...)
}

// Print prints to output.
func (s *Shell) Print(val ...interface{}) {
	s.reader.buf.Truncate(0)
	fmt.Fprint(s.reader.buf, val...)
	fmt.Fprint(s.writer, val...)
}

// Register registers a function for command. It overwrites existing function, if any.
// Use before (*Shell).Start.
func (s *Shell) Register(command string, function CmdFunc) (err error) {
	s.functions[command] = function
	// readline library does not provide a better way
	// yet than to regenerate the AutoComplete
	// TODO modify when available
	var pcItems []readline.PrefixCompleterInterface
	for word := range s.functions {
		pcItems = append(pcItems, readline.PcItem(word))
	}
	config := s.reader.scanner.Config
	config.AutoComplete = readline.NewPrefixCompleter(pcItems...)
	s.reader.scanner, err = readline.NewEx(config)
	return
}

// Unregister unregisters a function for a command
func (s *Shell) Unregister(command string) {
	delete(s.functions, command)
}

// RegisterGeneric registers a generic function for all inputs.
// It is called if the shell input could not be handled by any of the
// registered functions. Unlike Register, the entire line is passed as
// first argument to CmdFunc.
func (s *Shell) RegisterGeneric(function CmdFunc) {
	s.generic = function
}

// RegisterInterrupt registers a function to handle keyboard interrupt.
func (s *Shell) RegisterInterrupt(function CmdFunc) {
	s.interrupt = function
}

// SetPrompt sets the prompt string. The string to be displayed before the cursor.
func (s *Shell) SetPrompt(prompt string) {
	s.reader.prompt = prompt
	s.reader.scanner.SetPrompt(s.reader.rlPrompt())
}

// SetMultiPrompt sets the prompt string used for multiple lines. The string to be displayed before
// the cursor; starting from the second line of input.
func (s *Shell) SetMultiPrompt(prompt string) {
	s.reader.multiPrompt = prompt
}

// ShowPrompt sets whether prompt should show when requesting input for ReadLine and ReadPassword.
// Defaults to true.
func (s *Shell) ShowPrompt(show bool) {
	s.reader.showPrompt = show
	s.reader.scanner.SetPrompt(s.reader.rlPrompt())
}

// SetHistoryPath sets where readlines history file location. Use an empty
// string to disable history file. It is empty by default.
// func (s *Shell) SetHistoryPath(path string) error {
// 	var err error
// 	// Using scanner.SetHistoryPath doesn't initialize things properly and
// 	// history file is never written. Simpler to just create a new readline
// 	// Instance.
// 	s.reader.scanner.Close()
// 	config := s.reader.scanner.Config
// 	config.HistoryFile = path
// 	s.reader.scanner, err = readline.NewEx(config)
// 	return err
// }

// SetHomeHistoryPath is a convenience method that sets the history path with a
// $HOME prepended path.
// func (s *Shell) SetHomeHistoryPath(path string) {
// 	home := os.Getenv("HOME")
// 	abspath := fmt.Sprintf("%s/%s", home, path)
// 	s.SetHistoryPath(abspath)
// }

// SetOut sets the writer to write outputs to.
func (s *Shell) SetOut(writer io.Writer) {
	s.writer = writer
}

// PrintCommands prints a space separated list of registered commands to the shell.
func (s *Shell) PrintCommands() {
	out := strings.Join(s.Commands(), " ")
	if out != "" {
		s.Println("Commands:")
		s.Println(out)
	}
}

// Commands returns a sorted list of all registered commands.
func (s *Shell) Commands() []string {
	var commands []string
	for command := range s.functions {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	return commands
}

// IgnoreCase specifies whether commands should not be case sensitive.
// Defaults to false i.e. commands are case sensitive.
// If true, commands must be registered in lower cases. e.g. shell.Register("cmd", ...)
func (s *Shell) IgnoreCase(ignore bool) {
	s.ignoreCase = ignore
}
