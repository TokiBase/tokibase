package geo

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewCommand creates the `geo` command (index, rebuild, drop).
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "geo",
		Short: "R*Tree index for geoPoint radius/bbox queries",
		Long: "Radius and bounding-box queries on geoPoint fields are served by\n" +
			"GET /api/collections/{collection}/records/near (see docs/modules/geo.md).\n" +
			"Without an index the endpoint scans the collection; `geo index` creates an SQLite R*Tree table\n" +
			"_geo_<collection>_<field> that is kept up to date by record hooks of the running server.\n" +
			"Rename of the collection or field makes the index stale: drop it and index again.",
	}
	mk := func(use, short string, run func(c *cobra.Command, col, field string) error) *cobra.Command {
		return &cobra.Command{
			Use: use + " <collection> <field>", Short: short, Args: cobra.ExactArgs(2), SilenceUsage: true,
			RunE: func(c *cobra.Command, a []string) error { return run(c, a[0], a[1]) },
		}
	}
	root.AddCommand(
		mk("index", "Create the R*Tree index for a geoPoint field and fill it", func(c *cobra.Command, col, f string) error {
			n, err := Index(app, col, f)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "indexed %d record(s) in %s\n", n, TableName(col, f))
			return nil
		}),
		mk("rebuild", "Empty and refill an existing index (after bulk imports or if it drifted)", func(c *cobra.Command, col, f string) error {
			n, err := Rebuild(app, col, f)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "rebuilt %s: %d record(s)\n", TableName(col, f), n)
			return nil
		}),
		mk("drop", "Drop the R*Tree index (queries fall back to the JSON bounding box)", func(c *cobra.Command, col, f string) error {
			if err := Drop(app, col, f); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "dropped %s\n", TableName(col, f))
			return nil
		}),
	)
	return root
}
