package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func newGroup() *cobra.Command {
	cmd := &cobra.Command{Use: "group", Short: "Group administration"}
	cmd.AddCommand(
		newGroupList(),
		newGroupAdd(),
		newGroupDelete(),
		newGroupAddUser(),
		newGroupRemoveUser(),
		newGroupMembers(),
	)
	return cmd
}

func unknownGroupErr(gid string) error {
	return fmt.Errorf("ncgo-cli: unknown group %q", gid)
}

func unknownUserErr(uid string) error {
	return fmt.Errorf("ncgo-cli: unknown user %q", uid)
}

// groupStore returns the users store, wiring the ADR-0098 member-keys hook
// when encryption.per_user_keys is on so CLI membership changes wrap and
// unwrap share file keys exactly as the server does. Hook failures surface
// as warnings on stderr; the membership change itself never fails on them.
func groupStore(cfg *config.Config, db database.DB) (*users.SQLStore, error) {
	us := users.NewSQLStore(db)
	if !cfg.Encryption.Enabled || !cfg.Encryption.PerUserKeys {
		return us, nil
	}
	current, previous, err := encrypt.LoadKeyring(cfg.Encryption.MasterKeyPath, cfg.Encryption.PreviousKeyPaths)
	if err != nil {
		return nil, fmt.Errorf("ncgo-cli: encryption: %w", err)
	}
	res, err := perUserResolver(db, cfg, current, previous)
	if err != nil {
		return nil, err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	us.MemberKeys = &files.KeySharer{
		Meta:    files.NewSQLStore(db),
		Wrapper: res,
		Shares:  sharing.NewSQLShareStore(db),
		Users:   us,
		Logger:  logger,
	}
	us.Logger = logger
	return us, nil
}

func newGroupList() *cobra.Command {
	var limit, offset int
	list := &cobra.Command{
		Use:   "list",
		Short: "List groups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			rows, err := users.NewSQLStore(db).ListGroups(ctx, limit, offset)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "GID\tDISPLAY NAME")
			for _, g := range rows {
				fmt.Fprintf(tw, "%s\t%s\n", g.GID, g.DisplayName)
			}
			return tw.Flush()
		},
	}
	list.Flags().IntVar(&limit, "limit", 0, "maximum groups to list (0 = all)")
	list.Flags().IntVar(&offset, "offset", 0, "skip this many groups")
	return list
}

func newGroupAdd() *cobra.Command {
	var display string
	add := &cobra.Command{
		Use:   "add <gid>",
		Short: "Create a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := users.NewSQLStore(db).CreateGroup(ctx, &users.Group{GID: args[0], DisplayName: display}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created group %s\n", args[0])
			return nil
		},
	}
	add.Flags().StringVar(&display, "display-name", "", "display name (default: the gid)")
	return add
}

func newGroupDelete() *cobra.Command {
	var yes bool
	del := &cobra.Command{
		Use:   "delete <gid>",
		Short: "Delete a group and its memberships",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("ncgo-cli: group delete requires --yes")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if err := users.NewSQLStore(db).DeleteGroup(ctx, args[0]); err != nil {
				if errors.Is(err, users.ErrNotFound) {
					return unknownGroupErr(args[0])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted group %s\n", args[0])
			return nil
		},
	}
	del.Flags().BoolVar(&yes, "yes", false, "confirm the deletion (required)")
	return del
}

func newGroupAddUser() *cobra.Command {
	return &cobra.Command{
		Use:   "adduser <gid> <uid>",
		Short: "Add a user to a group",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			us, err := groupStore(cfg, db)
			if err != nil {
				return err
			}
			if err := us.AddGroupMember(ctx, args[0], args[1]); err != nil {
				if errors.Is(err, users.ErrNotFound) {
					return fmt.Errorf("ncgo-cli: unknown group %q or user %q", args[0], args[1])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s to %s\n", args[1], args[0])
			return nil
		},
	}
}

func newGroupRemoveUser() *cobra.Command {
	return &cobra.Command{
		Use:   "removeuser <gid> <uid>",
		Short: "Remove a user from a group",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			us, err := groupStore(cfg, db)
			if err != nil {
				return err
			}
			if err := us.RemoveGroupMember(ctx, args[0], args[1]); err != nil {
				if errors.Is(err, users.ErrNotFound) {
					return fmt.Errorf("ncgo-cli: unknown group %q or user %q", args[0], args[1])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", args[1], args[0])
			return nil
		},
	}
}

func newGroupMembers() *cobra.Command {
	var limit int
	members := &cobra.Command{
		Use:   "members <gid>",
		Short: "List the members of a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			uids, err := users.NewSQLStore(db).GroupMembers(ctx, args[0], limit)
			if err != nil {
				if errors.Is(err, users.ErrNotFound) {
					return unknownGroupErr(args[0])
				}
				return err
			}
			w := cmd.OutOrStdout()
			for _, uid := range uids {
				fmt.Fprintln(w, uid)
			}
			return nil
		},
	}
	members.Flags().IntVar(&limit, "limit", 0, "maximum members to list (0 = all)")
	return members
}
