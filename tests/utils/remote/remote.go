package remote

import (
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// Client - wrapper to run bash commands over ssh
type Client struct {
	// ConnectionString - user@host for ssh command
	ConnectionString string

	// CMDWaitInterval - run command every N seconds to check the output
	CMDWaitInterval time.Duration

	// CMDWaitTimeout - consider command to fail after this timeout exceeded
	CMDWaitTimeout time.Duration
}

func (c *Client) String() string {
	return c.ConnectionString
}

// Exec - run command over ssh
func (c *Client) Exec(cmd string) (string, error) {
	fmt.Printf("%v SSH exec: %v\n", c.ConnectionString, cmd)
	out, err := exec.Command("ssh", c.ConnectionString, cmd).Output()
	if err != nil {
		return "", fmt.Errorf("Command 'ssh %v, %v' error: %v", c.ConnectionString, cmd, err)
	}
	return fmt.Sprintf("%s", out), nil
}

// ExecAndWaitRegExp - wait command output to to satisfy regex or return error on timeout
func (c *Client) ExecAndWaitRegExp(cmd string, re *regexp.Regexp, inverted bool) error {
	done := make(chan error)
	timer := time.NewTimer(0)
	timeout := time.After(c.CMDWaitTimeout)
	lastOutput := ""

	go func() {
		startTime := time.Now()
		for {
			select {
			case <-timer.C:
				out, err := c.Exec(cmd)
				if err != nil {
					done <- err
					return
				} else if (!inverted && re.MatchString(out)) || (inverted && !re.MatchString(out)) {
					done <- nil
					return
				}

				lastOutput = out
				waitingTimeSeconds := time.Since(startTime).Seconds()
				if waitingTimeSeconds >= c.CMDWaitInterval.Seconds() {
					fmt.Printf("...waiting cmd for %.0fs\n", waitingTimeSeconds)
				}
				timer = time.NewTimer(c.CMDWaitInterval)
			case <-timeout:
				timer.Stop()
				done <- fmt.Errorf(
					"Checking cmd output timeout exceeded (%v), "+
						"cmd: '%v', regexp: '%v', inverted: %v, last output:\n"+
						"---\n%v\n---\n",
					c.CMDWaitTimeout,
					cmd,
					re,
					inverted,
					lastOutput,
				)
				return
			}
		}
	}()

	return <-done
}

// CopyFiles - copy local files to remote server
func (c *Client) CopyFiles(from, to string) error {
	toAddress := fmt.Sprintf("%v:%v", c.ConnectionString, to)

	fmt.Printf("%v SCP from: %v, to %v\n", c.ConnectionString, from, toAddress)

	if _, err := exec.Command("scp", from, toAddress).Output(); err != nil {
		return fmt.Errorf("Command 'scp %v %v' error: %v", from, toAddress, err)
	}

	return nil
}
