// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package dockercontainer

import (
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/executers"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/framework/runpluginutil"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/plugins/pluginutil"
	"github.com/aws/amazon-ssm-agent/agent/rebooter"
	"github.com/aws/amazon-ssm-agent/agent/task"
)

const (
	//Action values
	CREATE  = "Create"
	START   = "Start"
	RUN     = "Run"
	STOP    = "Stop"
	RM      = "Rm"
	EXEC    = "Exec"
	INSPECT = "Inspect"
	LOGS    = "Logs"
	PS      = "Ps"
	STATS   = "Stats"
	PULL    = "Pull"
	IMAGES  = "Images"
	RMI     = "Rmi"
)
const (
	ACTION_REQUIRES_PARAMETER = "Action %s requires parameter %s"
)

var dockerExecCommand = "docker.exe"
var duration_Seconds time.Duration = 30 * time.Second

// Plugin is the type for the plugin.
type Plugin struct {
	pluginutil.DefaultPlugin
}

// RunCommandPluginInput represents one set of commands executed by the RunCommand plugin.
type DockerContainerPluginInput struct {
	contracts.PluginInput
	Action           string
	ID               string
	WorkingDirectory string
	TimeoutSeconds   interface{}
	Container        string
	Cmd              string
	Image            string
	Memory           string
	CpuShares        string
	Volume           []string
	Env              string
	User             string
	Publish          string
}

// PSModulePluginOutput represents the output of the plugin
type DockerContainerPluginOutput struct {
	contracts.PluginOutput
}

// Failed marks plugin as Failed
func (out *DockerContainerPluginOutput) MarkAsFailed(log log.T, err error) {
	out.ExitCode = 1
	out.Status = contracts.ResultStatusFailed
	if len(out.Stderr) != 0 {
		out.Stderr = fmt.Sprintf("\n%v\n%v", out.Stderr, err.Error())
	} else {
		out.Stderr = fmt.Sprintf("\n%v", err.Error())
	}
	log.Error(err.Error())
	out.Errors = append(out.Errors, err.Error())
}

// NewPlugin returns a new instance of the plugin.
func NewPlugin(pluginConfig pluginutil.PluginConfig) (*Plugin, error) {
	var plugin Plugin
	var err error
	plugin.MaxStdoutLength = pluginConfig.MaxStdoutLength
	plugin.MaxStderrLength = pluginConfig.MaxStderrLength
	plugin.StdoutFileName = pluginConfig.StdoutFileName
	plugin.StderrFileName = pluginConfig.StderrFileName
	plugin.OutputTruncatedSuffix = pluginConfig.OutputTruncatedSuffix
	plugin.Uploader = pluginutil.GetS3Config()
	plugin.ExecuteUploadOutputToS3Bucket = pluginutil.UploadOutputToS3BucketExecuter(plugin.UploadOutputToS3Bucket)

	exec := executers.ShellCommandExecuter{}
	plugin.ExecuteCommand = pluginutil.CommandExecuter(exec.Execute)

	return &plugin, err
}

// Name returns the name of the plugin
func Name() string {
	return appconfig.PluginNameDockerContainer
}

// Execute runs multiple sets of commands and returns their outputs.
// res.Output will contain a slice of DockerContainerPluginOutput.
func (p *Plugin) Execute(context context.T, config contracts.Configuration, cancelFlag task.CancelFlag, pluginRunner runpluginutil.PluginRunner) (res contracts.PluginResult) {
	log := context.Log()
	log.Infof("%v started with configuration %v", Name(), config)
	res.StartDateTime = time.Now()
	defer func() {
		res.EndDateTime = time.Now()
	}()

	//loading Properties as list since aws:psModule uses properties as list
	var properties []interface{}
	if properties, res = pluginutil.LoadParametersAsList(log, config.Properties); res.Code != 0 {

		pluginutil.PersistPluginInformationToCurrent(log, config.PluginID, config, res)
		return res
	}

	out := make([]DockerContainerPluginOutput, len(properties))
	for i, prop := range properties {
		// check if a reboot has been requested
		if rebooter.RebootRequested() {
			log.Info("A plugin has requested a reboot.")
			return
		}

		if cancelFlag.ShutDown() {
			out[i].ExitCode = 1
			out[i].Status = contracts.ResultStatusFailed
			break
		}

		if cancelFlag.Canceled() {
			out[i].ExitCode = 1
			out[i].Status = contracts.ResultStatusCancelled
			break
		}

		out[i] = p.runCommandsRawInput(log, prop, config.OrchestrationDirectory, cancelFlag, config.OutputS3BucketName, config.OutputS3KeyPrefix)
	}

	if len(properties) > 0 {
		res.Code = out[0].ExitCode
		res.Status = out[0].Status
		res.Output = out[0].String()
	}

	pluginutil.PersistPluginInformationToCurrent(log, config.PluginID, config, res)

	return res
}

// runCommandsRawInput executes one set of commands and returns their output.
// The input is in the default json unmarshal format (e.g. map[string]interface{}).
func (p *Plugin) runCommandsRawInput(log log.T, rawPluginInput interface{}, orchestrationDirectory string, cancelFlag task.CancelFlag, outputS3BucketName string, outputS3KeyPrefix string) (out DockerContainerPluginOutput) {
	var pluginInput DockerContainerPluginInput
	err := jsonutil.Remarshal(rawPluginInput, &pluginInput)
	log.Debugf("Plugin input %v", pluginInput)
	if err != nil {
		errorString := fmt.Errorf("Invalid format in plugin properties %v;\nerror %v", rawPluginInput, err)
		out.MarkAsFailed(log, errorString)
		return
	}

	return p.runCommands(log, pluginInput, orchestrationDirectory, cancelFlag, outputS3BucketName, outputS3KeyPrefix)
}

// runCommands executes one set of commands and returns their output.
func (p *Plugin) runCommands(log log.T, pluginInput DockerContainerPluginInput, orchestrationDirectory string, cancelFlag task.CancelFlag, outputS3BucketName string, outputS3KeyPrefix string) (out DockerContainerPluginOutput) {
	var err error

	// if no orchestration directory specified, create temp directory
	var useTempDirectory = (orchestrationDirectory == "")
	var tempDir string
	if useTempDirectory {
		if tempDir, err = ioutil.TempDir("", "Ec2RunCommand"); err != nil {
			out.MarkAsFailed(log, err)
			return
		}
		orchestrationDirectory = tempDir
	}

	orchestrationDir := fileutil.BuildPath(orchestrationDirectory, pluginInput.ID)
	log.Debugf("OrchestrationDir %v ", orchestrationDir)

	if err = validateInputs(pluginInput); err != nil {
		log.Error("Validation error", err)
		out.MarkAsFailed(log, err)
		return out
	}

	// create orchestration dir if needed
	if err = fileutil.MakeDirs(orchestrationDir); err != nil {
		log.Debug("failed to create orchestrationDir directory", orchestrationDir, err)
		out.MarkAsFailed(log, err)
		return
	}
	var commandName string = "docker"
	var commandArguments []string
	switch pluginInput.Action {
	case CREATE, RUN:
		if len(pluginInput.Image) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "image")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "image"))

			return out
		}
		commandArguments = make([]string, 0)
		if pluginInput.Action == RUN {
			commandArguments = append(commandArguments, "run", "-d")
		} else {
			commandArguments = append(commandArguments, "create")
		}
		if len(pluginInput.Volume) > 0 && len(pluginInput.Volume[0]) > 0 {
			out.Stdout += "pluginInput.Volume:" + strconv.Itoa(len(pluginInput.Volume))

			log.Info("pluginInput.Volume", len(pluginInput.Volume))
			commandArguments = append(commandArguments, "--volume")
			for _, vol := range pluginInput.Volume {
				log.Info("pluginInput.Volume item", vol)
				commandArguments = append(commandArguments, vol)
			}
		}
		if len(pluginInput.Container) > 0 {
			commandArguments = append(commandArguments, "--name")
			commandArguments = append(commandArguments, pluginInput.Container)
		}
		if len(pluginInput.Memory) > 0 {
			commandArguments = append(commandArguments, "--memory")
			commandArguments = append(commandArguments, pluginInput.Memory)
		}
		if len(pluginInput.CpuShares) > 0 {
			commandArguments = append(commandArguments, "--cpu-shares")
			commandArguments = append(commandArguments, pluginInput.CpuShares)
		}
		if len(pluginInput.Publish) > 0 {
			commandArguments = append(commandArguments, "--publish")
			commandArguments = append(commandArguments, pluginInput.Publish)
		}
		if len(pluginInput.Env) > 0 {
			commandArguments = append(commandArguments, "--env")
			commandArguments = append(commandArguments, pluginInput.Env)
		}
		if len(pluginInput.User) > 0 {
			commandArguments = append(commandArguments, "--user")
			commandArguments = append(commandArguments, pluginInput.User)
		}
		commandArguments = append(commandArguments, pluginInput.Image)
		commandArguments = append(commandArguments, pluginInput.Cmd)

	case START:
		commandArguments = append(commandArguments, "start")
		if len(pluginInput.Container) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container"))
			return out
		}
		commandArguments = append(commandArguments, pluginInput.Container)

	case RM:
		commandArguments = append(commandArguments, "rm")
		if len(pluginInput.Container) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container"))
			return out
		}
		commandArguments = append(commandArguments, pluginInput.Container)

	case STOP:
		commandArguments = append(commandArguments, "stop")
		if len(pluginInput.Container) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container"))
			return out
		}
		commandArguments = append(commandArguments, pluginInput.Container)

	case EXEC:
		commandArguments = append(commandArguments, "exec")
		if len(pluginInput.Container) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container"))
			return out
		}
		if len(pluginInput.Cmd) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "cmd")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "cmd"))
			return out
		}
		if len(pluginInput.User) > 0 {
			commandArguments = append(commandArguments, "--user")
			commandArguments = append(commandArguments, pluginInput.User)
		}
		commandArguments = append(commandArguments, pluginInput.Container)
		commandArguments = append(commandArguments, pluginInput.Cmd)
	case INSPECT:
		commandArguments = append(commandArguments, "inspect")
		if len(pluginInput.Container) == 0 && len(pluginInput.Image) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container or image")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container or image"))
			return out
		}
		commandArguments = append(commandArguments, pluginInput.Container)
		commandArguments = append(commandArguments, pluginInput.Image)
	case STATS:
		commandArguments = append(commandArguments, "stats")
		commandArguments = append(commandArguments, "--no-stream")
		if len(pluginInput.Container) > 0 {
			commandArguments = append(commandArguments, pluginInput.Container)
		}
	case LOGS:
		commandArguments = append(commandArguments, "logs")
		if len(pluginInput.Container) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "container"))
			return out
		}
		commandArguments = append(commandArguments, pluginInput.Container)
	case PULL:
		commandArguments = append(commandArguments, "pull")
		if len(pluginInput.Image) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "image")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "image"))
			return out
		}
		commandArguments = append(commandArguments, pluginInput.Image)
	case IMAGES:
		commandArguments = append(commandArguments, "images")
	case RMI:
		commandArguments = append(commandArguments, "rmi")
		if len(pluginInput.Image) == 0 {
			log.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "image")
			out.MarkAsFailed(log, fmt.Errorf(ACTION_REQUIRES_PARAMETER, pluginInput.Action, "image"))
			return out
		}
		commandArguments = append(commandArguments, pluginInput.Image)

	case PS:
		commandArguments = append(commandArguments, "ps", "--all")
	default:
		out.MarkAsFailed(log, fmt.Errorf("Docker Action is set to unsupported value: %v", pluginInput.Action))
		return out
	}

	executionTimeout := pluginutil.ValidateExecutionTimeout(log, pluginInput.TimeoutSeconds)
	// Create output file paths
	stdoutFilePath := filepath.Join(orchestrationDir, p.StdoutFileName)
	stderrFilePath := filepath.Join(orchestrationDir, p.StderrFileName)
	log.Debugf("stdout file %v, stderr file %v", stdoutFilePath, stderrFilePath)

	// Execute Command
	stdout, stderr, exitCode, errs := p.ExecuteCommand(log, pluginInput.WorkingDirectory, stdoutFilePath, stderrFilePath, cancelFlag, executionTimeout, commandName, commandArguments)

	// Set output status
	out.ExitCode = exitCode
	out.Status = pluginutil.GetStatus(out.ExitCode, cancelFlag)

	if len(errs) > 0 {
		for _, err := range errs {
			out.Errors = append(out.Errors, err.Error())
			if out.Status != contracts.ResultStatusCancelled &&
				out.Status != contracts.ResultStatusTimedOut &&
				out.Status != contracts.ResultStatusSuccessAndReboot {
				log.Error("failed to run commands: ", err)
				out.Status = contracts.ResultStatusFailed
			}
		}
	}

	// read (a prefix of) the standard output/error
	out.Stdout, err = pluginutil.ReadPrefix(stdout, p.MaxStdoutLength, p.OutputTruncatedSuffix)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		log.Error(err)
	}
	out.Stderr, err = pluginutil.ReadPrefix(stderr, p.MaxStderrLength, p.OutputTruncatedSuffix)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		log.Error(err)
	}

	// Upload output to S3
	uploadOutputToS3BucketErrors := p.ExecuteUploadOutputToS3Bucket(log, pluginInput.ID, orchestrationDir, outputS3BucketName, outputS3KeyPrefix, useTempDirectory, tempDir, out.Stdout, out.Stderr)
	out.Errors = append(out.Errors, uploadOutputToS3BucketErrors...)

	// Return Json indented response
	responseContent, _ := jsonutil.Marshal(out)
	log.Debug("Returning response:\n", jsonutil.Indent(responseContent))
	return out
}

func validateInputs(pluginInput DockerContainerPluginInput) (err error) {
	validContainerName := regexp.MustCompile(`^[a-zA-Z0-9_\-\\\/]*$`)
	if !validContainerName.MatchString(pluginInput.Container) {
		return errors.New("Invalid container name, only [a-zA-Z0-9_-] are allowed")
	}
	validImageValue := regexp.MustCompile(`^[a-zA-Z0-9_\-\\\/]*$`)
	if !validImageValue.MatchString(pluginInput.Image) {
		return errors.New("Invalid image value, only [a-zA-Z0-9_-] are allowed")
	}
	validUserValue := regexp.MustCompile(`^[a-zA-Z0-9_-]*$`)
	if !validUserValue.MatchString(pluginInput.User) {
		return errors.New("Invalid user value")
	}
	validPathName := regexp.MustCompile(`^[\w\\\/_\:\-\.\"\(\)\^ ]*$`)
	for _, vol := range pluginInput.Volume {
		if !validPathName.MatchString(vol) {
			return errors.New("Invalid volume")
		}
	}
	validCpuSharesValue := regexp.MustCompile(`^/?[a-zA-Z0-9_-]*$`)
	if !validCpuSharesValue.MatchString(pluginInput.CpuShares) {
		return errors.New("Invalid CpuShares value, only integars are allowed")
	}
	validMemoryValue := regexp.MustCompile(`^[0-9]*[bkmg]?$`)
	if !validMemoryValue.MatchString(pluginInput.Memory) {
		return errors.New("Invalid CpuShares value")
	}
	validPublishValue := regexp.MustCompile(`^[0-9a-zA-Z:\-\/.]*$`)
	if !validPublishValue.MatchString(pluginInput.Publish) {
		return errors.New("Invalid Publish value")
	}
	blacklist := regexp.MustCompile(`[;,&|]+`)
	if blacklist.MatchString(pluginInput.Env) {
		return errors.New("Invalid environment variable value")
	}
	if blacklist.MatchString(pluginInput.Cmd) {
		return errors.New("Invalid command value")
	}

	return err
}