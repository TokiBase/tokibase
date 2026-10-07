package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func parseUser(s string) (UserRef, error) {
	col, id, ok := strings.Cut(s, ":")
	if !ok || col == "" || id == "" {
		return UserRef{}, errors.New("user must be collection:id")
	}
	return UserRef{Collection: col, ID: id}, nil
}

// NewCommand returns the `push` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "push", Short: "Manage push devices and send notifications (FCM, APNs)"}

	var user string
	var asJSON bool
	devices := &cobra.Command{
		Use: "devices", Short: "List registered devices (tokens are shortened)", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := ensureCollections(app); err != nil {
				return err
			}
			col, id := "", ""
			if user != "" {
				u, err := parseUser(user)
				if err != nil {
					return err
				}
				col, id = u.Collection, u.ID
			}
			list, err := ListDevices(app, col, id)
			if err != nil {
				return err
			}
			if asJSON {
				out := make([]Device, 0, len(list))
				for _, d := range list {
					out = append(out, d.Masked())
				}
				return printJSON(c.OutOrStdout(), out)
			}
			for _, d := range list {
				state := "enabled"
				if !d.Enabled {
					state = "disabled"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s:%s\t%s\t%s\tapp=%s\tlast_seen=%s\t%s\n",
					d.ID, d.Collection, d.Record, d.Platform, state, d.AppID, d.LastSeen, MaskToken(d.Token))
			}
			return nil
		},
	}
	devices.Flags().StringVar(&user, "user", "", "only devices of collection:id")
	devices.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var toUser, topic, token, title, body, data string
	send := &cobra.Command{
		Use: "send", Short: "Queue a notification (a running server delivers it)", SilenceUsage: true,
		Example: `  toki push send --to-user users:abc123 --title Hello --body "It works" --data '{"screen":"inbox"}'
  toki push send --topic news --title "New post"`,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := ensureCollections(app); err != nil {
				return err
			}
			m, err := moduleOf(app)
			if err != nil {
				return err
			}
			_ = m
			var t Target
			if toUser != "" {
				u, err := parseUser(toUser)
				if err != nil {
					return err
				}
				t.Users = []UserRef{u}
			}
			if topic != "" {
				t.Topics = []string{topic}
			}
			if token != "" {
				t.Tokens = []string{token}
			}
			if len(t.Users)+len(t.Topics)+len(t.Tokens) != 1 {
				return errors.New("give exactly one of --to-user, --topic, --token")
			}
			n := Notification{Title: title, Body: body}
			if data != "" {
				var raw map[string]any
				if err := json.Unmarshal([]byte(data), &raw); err != nil {
					return fmt.Errorf("--data must be a JSON object: %w", err)
				}
				if n.Data, err = stringifyData(raw); err != nil {
					return err
				}
			}
			q, err := sendFrom(app, Message{To: t, Notification: n}, "cli")
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "queued %d job(s); a running server delivers them (see `toki jobs list`)\n", q)
			return nil
		},
	}
	send.Flags().StringVar(&toUser, "to-user", "", "recipient auth record, collection:id")
	send.Flags().StringVar(&topic, "topic", "", "recipient topic")
	send.Flags().StringVar(&token, "token", "", "recipient device token (must be registered)")
	send.Flags().StringVar(&title, "title", "", "notification title")
	send.Flags().StringVar(&body, "body", "", "notification body")
	send.Flags().StringVar(&data, "data", "", "custom data, JSON object")

	test := &cobra.Command{
		Use: "test", Short: "Print provider configuration and send a dry run to the fake provider", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			m, err := moduleOf(app)
			if err != nil {
				return err
			}
			st := m.ProviderStatus()
			for _, pl := range []string{PlatformFCM, PlatformAPNs} {
				fmt.Fprintf(c.OutOrStdout(), "%s: %s\n", pl, st[pl])
			}
			f := NewFake("fake")
			n := &Notification{Title: "toki push test", Body: "dry run", Data: map[string]string{"k": "v"}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := f.Send(ctx, n, "dry-run-token"); err != nil {
				return err
			}
			if _, err := fcmBodyJSON(n); err != nil {
				return err
			}
			if _, err := apnsPayload(n); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "dry run: payloads built and delivered to the fake provider (nothing sent to FCM/APNs)")
			return nil
		},
	}

	var days int
	prune := &cobra.Command{
		Use: "prune", Short: "Delete devices not seen for 90 days (--days to change)", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := ensureCollections(app); err != nil {
				return err
			}
			n, err := PruneDevices(app, time.Duration(days)*24*time.Hour)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "deleted %d device(s)\n", n)
			return nil
		},
	}
	prune.Flags().IntVar(&days, "days", 90, "age in days")

	var desc string
	var public bool
	topicCmd := &cobra.Command{Use: "topic", Short: "Manage topics"}
	topicAdd := &cobra.Command{
		Use: "add <name>", Short: "Create a topic", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensureCollections(app); err != nil {
				return err
			}
			vis := VisibilityPrivate
			if public {
				vis = VisibilityPublic
			}
			if err := CreateTopicVisibility(app, args[0], desc, vis); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "topic %q ready\n", args[0])
			return nil
		},
	}
	topicAdd.Flags().StringVar(&desc, "description", "", "description")
	topicAdd.Flags().BoolVar(&public, "public", false, "let end users list and subscribe (default: private)")
	topicList := &cobra.Command{
		Use: "list", Short: "List topics", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := ensureCollections(app); err != nil {
				return err
			}
			ts, err := ListTopics(app)
			if err != nil {
				return err
			}
			for _, t := range ts {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\n", t.Name, t.Visibility, t.Description)
			}
			return nil
		},
	}
	topicCmd.AddCommand(topicAdd, topicList)

	root.AddCommand(devices, send, test, prune, topicCmd)
	return root
}
