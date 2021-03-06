package cmd

import (
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/spf13/cobra"

	libecs "github.com/SKAhack/shipctl/lib/ecs"
	log "github.com/SKAhack/shipctl/lib/logger"
)

type rollbackCmd struct {
	cluster         string
	serviceName     string
	backend         string
	slackWebhookUrl string
}

func NewRollbackCommand(out, errOut io.Writer) *cobra.Command {
	f := &rollbackCmd{}
	cmd := &cobra.Command{
		Use:   "rollback [options]",
		Short: "",
		RunE: func(cmd *cobra.Command, args []string) error {
			l := log.NewLogger(f.cluster, f.serviceName, f.slackWebhookUrl, out)
			err := f.execute(cmd, args, l)
			if err != nil {
				msg := fmt.Sprintf("failed to deploy. cluster: %s, serviceName: %s\n", f.cluster, f.serviceName)
				l.Log(msg)
				l.Slack("danger", msg)
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&f.cluster, "cluster", "", "ECS Cluster Name")
	cmd.Flags().StringVar(&f.serviceName, "service-name", "", "ECS Service Name")
	cmd.Flags().StringVar(&f.backend, "backend", "SSM", "Backend type of state manager")
	cmd.Flags().StringVar(&f.slackWebhookUrl, "slack-webhook-url", "", "slack webhook URL")

	return cmd
}

func (f *rollbackCmd) execute(_ *cobra.Command, args []string, l *log.Logger) error {
	if f.cluster == "" {
		return errors.New("--cluster is required")
	}

	if f.serviceName == "" {
		return errors.New("--service-name is required")
	}

	region := getAWSRegion()
	if region == "" {
		return errors.New("AWS region is not found. please set a AWS_DEFAULT_REGION or AWS_REGION")
	}

	sess, err := session.NewSession()
	if err != nil {
		return err
	}

	client := ecs.New(sess, &aws.Config{
		Region: aws.String(region),
	})

	historyManager, err := NewHistoryManager(f.backend, f.cluster, f.serviceName)
	if err != nil {
		return err
	}

	states, err := historyManager.Pull()
	if err != nil {
		return err
	}
	if len(states) < 2 {
		return errors.New("can not found a prev state")
	}

	prevState := states[len(states)-2]
	state := states[len(states)-1]

	service, err := libecs.DescribeService(client, f.cluster, f.serviceName)
	if err != nil {
		return err
	}

	if len(service.Deployments) > 1 {
		return errors.New(fmt.Sprintf("%s is currently deploying", f.serviceName))
	}

	var taskDef *ecs.TaskDefinition
	{
		taskDefArn := *service.TaskDefinition
		taskDefArn, err = libecs.SpecifyRevision(prevState.Revision, taskDefArn)
		if err != nil {
			return err
		}

		taskDef, err = libecs.DescribeTaskDefinition(client, taskDefArn)
		if err != nil {
			return err
		}
	}

	var msg string
	msg = fmt.Sprintf("rollback: revision %d -> %d\n", state.Revision, prevState.Revision)
	l.Log(msg)
	l.Slack("normal", msg)

	err = libecs.UpdateService(client, service, taskDef)
	if err != nil {
		return err
	}

	l.Log(fmt.Sprintf("service updating\n"))

	err = libecs.WaitUpdateService(client, f.cluster, f.serviceName, l)
	if err != nil {
		return err
	}

	err = historyManager.PushState(
		prevState.Revision,
		fmt.Sprintf("rollback: %d -> %d", state.Revision, prevState.Revision),
	)
	if err != nil {
		return err
	}

	msg = fmt.Sprintf("successfully updated\n")
	l.Log(msg)
	l.Slack("good", msg)

	return nil
}
