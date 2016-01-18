package command

import (
	"fmt"
	"os"
	"os/exec"
	"text/template"

	log "github.com/funkygao/log4go"
)

//go:generate go-bindata -nomemcopy -pkg command templates/...

type BackendServers struct {
	CpuNum      int
	HaproxyRoot string
	LogDir      string

	Pub []Backend
	Sub []Backend
	Man []Backend
}

func (this *BackendServers) reset() {
	this.Pub = make([]Backend, 0)
	this.Sub = make([]Backend, 0)
	this.Man = make([]Backend, 0)
}

type Backend struct {
	Name string
	Addr string
}

func (this *Start) createConfigFile(servers BackendServers) error {
	log.Info("backends: %+v", servers)

	tmpFile := fmt.Sprintf("%s.tmp", configFile)
	cfgFile, err := os.Create(tmpFile)
	if err != nil {
		return err
	}
	defer cfgFile.Close()

	b, _ := Asset("templates/haproxy.tpl")
	t := template.Must(template.New("haproxy").Parse(string(b)))

	err = t.Execute(cfgFile, servers)
	if err != nil {
		return err
	}

	return os.Rename(tmpFile, configFile)
}

func (this *Start) reloadHAproxy() (err error) {
	var cmd *exec.Cmd = nil
	waitStartCh := make(chan struct{})
	if this.pid == -1 {
		log.Info("starting haproxy")
		cmd = exec.Command(this.command, "-f", configFile)
		go func() {
			<-waitStartCh
			if err := cmd.Wait(); err != nil {
				log.Error("haproxy: %v", err)
			}
		}()
	} else {
		shellScript := fmt.Sprintf("%s -f %s/%s -sf `cat %s/haproxy.pid`",
			this.command, this.root, configFile, this.root)
		log.Info("reloading: %s", shellScript)
		cmd = exec.Command("/bin/sh", "-c", shellScript)
		go func() {
			<-waitStartCh
			if err := cmd.Wait(); err != nil {
				log.Error("haproxy: %v", err)
			}
		}()
	}

	if err = cmd.Start(); err == nil {
		waitStartCh <- struct{}{}

		this.pid = cmd.Process.Pid
		log.Info("haproxy started with pid: %d", this.pid)
	}

	return err
}
