package main

import (
	"fmt"

	cfg "github.com/grmrgecko/repo-sync/config"
	"github.com/kardianos/service"
)

// systemdScript is the unit template used when installing the service. It
// replaces the library default to run as a notify service with automatic
// restart, so systemd only considers the mirror started once the listener
// is bound.
const systemdScript = `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}
{{range Dependencies}}{{.}}
{{end}}StartLimitIntervalSec=500
StartLimitBurst=5

[Service]
Type=notify
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if ChRoot}}RootDirectory={{ChRoot | cmd}}
{{end}}{{if WorkingDirectory}}WorkingDirectory={{WorkingDirectory | cmdEscape}}
{{end}}{{if UserName}}User={{UserName}}
{{end}}{{if ReloadSignal}}ExecReload=/bin/kill -{{ReloadSignal}} "$MAINPID"
{{end}}{{if PIDFile}}PIDFile={{PIDFile | cmd}}
{{end}}{{if OutputFileSupport}}StandardOutput=file:{{LogDirectory}}/{{Name}}.out
StandardError=file:{{LogDirectory}}/{{Name}}.err
{{end}}{{if LimitNOFILE}}LimitNOFILE={{LimitNOFILE}}
{{end}}{{if Restart}}Restart={{Restart}}
{{end}}{{if SuccessExitStatus}}SuccessExitStatus={{SuccessExitStatus}}
{{end}}RestartSec=5
EnvironmentFile=-/etc/sysconfig/{{Name}}

{{range EnvVars}}{{.}}
{{end}}[Install]
WantedBy=multi-user.target
`

// ServiceAction lists the accepted service actions, in the order the Run
// switch handles them.
var ServiceAction = []string{"start", "stop", "status", "restart", "install", "uninstall"}

// ServiceCmd manages the mirror server as a system service.
type ServiceCmd struct {
	Action string `arg:"" enum:"${serviceActions}" help:"${serviceActions}" required:""`
}

// action returns the requested service action.
func (s *ServiceCmd) action() string {
	return s.Action
}

// Run performs the requested action against the installed service.
func (s *ServiceCmd) Run() (err error) {
	svc, err := s.service()
	if err != nil {
		return err
	}
	switch s.action() {
	case ServiceAction[0]:
		err = svc.Start()
	case ServiceAction[1]:
		err = svc.Stop()
	case ServiceAction[2]:
		var status service.Status
		status, err = svc.Status()
		if err == nil {
			switch status {
			case service.StatusRunning:
				fmt.Println("Service is running.")
			case service.StatusStopped:
				fmt.Println("Service is stopped.")
			default:
				fmt.Println("Service is in an unknown state.")
			}
		}
	case ServiceAction[3]:
		err = svc.Restart()
	case ServiceAction[4]:
		err = svc.Install()
	case ServiceAction[5]:
		err = svc.Uninstall()
	}
	if err != nil {
		return err
	}
	// Status already printed its own result.
	if s.action() != ServiceAction[2] {
		fmt.Println("Command executed successfully.")
	}
	return
}

// service builds the service definition shared by the management actions
// and by the server when it is started by the service manager.
func (s *ServiceCmd) service() (service.Service, error) {
	svcConfig := &service.Config{
		Name:         cfg.Name,
		DisplayName:  cfg.DisplayName,
		Description:  cfg.Description,
		Arguments:    []string{"server"},
		Dependencies: []string{"After=network.target"},
		Option: service.KeyValue{
			"SystemdScript": systemdScript,
			"ReloadSignal":  "HUP",
			"Restart":       "always",
		},
	}
	return service.New(s, svcConfig)
}

// Start satisfies service.Interface. The server is already running in the
// foreground by the time the supervisor attaches.
func (s *ServiceCmd) Start(svc service.Service) error {
	return nil
}

// Stop satisfies service.Interface, signalling the server's shutdown.
func (s *ServiceCmd) Stop(svc service.Service) error {
	stopChan <- struct{}{}
	return nil
}
