//+build integration
//+build darwin dragonfly freebsd linux netbsd openbsd solaris
// only posix systems ^

package tests_test

import (
	"bufio"
	"flag"
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/foxcpp/maddy/tests"
)

var DovecotExecutable string

func init() {
	flag.StringVar(&DovecotExecutable, "integration.dovecot", "dovecot", "path to dovecot executable for interop tests")
}

const dovecotConf = `base_dir = $ROOT/run/
log_path = /dev/stderr
ssl = no

default_internal_user = $USER
default_internal_group = $GROUP
default_login_user = $USER

passdb {
	driver = passwd-file
	args = $ROOT/passwd
}

userdb {
	driver = passwd-file
	args = $ROOT/passwd
}

service auth {
	unix_listener auth {
		mode = 0666
	}
}

# Dovecot refuses to start without protocols, so we need to give it one.
protocols = imap

service imap-login {
	chroot =
	inet_listener imap {
		address = 127.0.0.1
		port = 0
	}
}

service anvil {
	chroot =
}

# Turn on debugging information, to help troubleshooting issues.
auth_verbose = yes
auth_debug = yes
auth_debug_passwords = yes
auth_verbose_passwords = yes
mail_debug = yes
`

const dovecotPasswd = `tester:{plain}123456:1000:1000::/home/user`

func runDovecot(t *testing.T) (string, *exec.Cmd) {
	dovecotExec, err := exec.LookPath(DovecotExecutable)
	if err != nil {
		if err == exec.ErrNotFound {
			t.Skip("No Dovecot executable found, skipping interop. tests")
		}
		t.Fatal(err)
	}

	tempDir, err := ioutil.TempDir("", "maddy-dovecot-interop-")
	if err != nil {
		t.Fatal(err)
	}

	curUser, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	curGroup, err := user.LookupGroupId(curUser.Gid)
	if err != nil {
		t.Fatal(err)
	}

	dovecotConf := strings.NewReplacer(
		"$ROOT", tempDir,
		"$USER", curUser.Username,
		"$GROUP", curGroup.Name).Replace(dovecotConf)
	err = ioutil.WriteFile(filepath.Join(tempDir, "dovecot.conf"), []byte(dovecotConf), os.ModePerm)
	if err != nil {
		t.Fatal(err)
	}
	err = ioutil.WriteFile(filepath.Join(tempDir, "passwd"), []byte(dovecotPasswd), os.ModePerm)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(dovecotExec, "-F", "-c", filepath.Join(tempDir, "dovecot.conf"))
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{}, 1)

	go func() {
		scnr := bufio.NewScanner(stderr)
		for scnr.Scan() {
			line := scnr.Text()

			// One of messages printed near completing initialization.
			if strings.Contains(line, "master: Error: file_dotlock_open(/var/lib/dovecot/instances) failed: Permission denied") {
				ready <- struct{}{}
			}

			t.Log("dovecot:", line)
		}
		if err := scnr.Err(); err != nil {
			t.Log("stderr I/O error:", err)
		}
	}()

	<-ready

	return tempDir, cmd
}

func cleanDovecot(t *testing.T, tempDir string, cmd *exec.Cmd) {
	cmd.Process.Signal(syscall.SIGTERM)
	if !t.Failed() {
		os.RemoveAll(tempDir)
	} else {
		t.Log("Dovecot directory is not deleted:", tempDir)
	}
}

func TestDovecotSASLClient(tt *testing.T) {
	tt.Parallel()

	dovecotDir, cmd := runDovecot(tt)
	defer cleanDovecot(tt, dovecotDir, cmd)

	t := tests.NewT(tt)
	t.DNS(nil)
	t.Port("smtp")
	t.Env("DOVECOT_SASL_SOCK=" + filepath.Join(dovecotDir, "run", "auth-client"))
	t.Config(`
		smtp tcp://127.0.0.1:{env:TEST_PORT_smtp} {
			hostname mx.maddy.test
			tls off
			auth dovecot_sasl unix://{env:DOVECOT_SASL_SOCK}
			deliver_to dummy
		}`)
	t.Run(1)
	defer t.Close()

	c := t.Conn("smtp")
	defer c.Close()
	c.SMTPNegotation("localhost", nil, nil)
	c.Writeln("AUTH PLAIN AHRlc3QAMTIzNDU2") // 0x00 test 0x00 123456 (invalid user)
	c.ExpectPattern("454 *")
	c.Writeln("AUTH PLAIN AHRlc3RlcgAxMjM0NQ==") // 0x00 tester 0x00 12345 (invalid password)
	c.ExpectPattern("454 *")
	c.Writeln("AUTH PLAIN AHRlc3RlcgAxMjM0NTY=") // 0x00 tester 0x00 123456
	c.ExpectPattern("235 *")
}
