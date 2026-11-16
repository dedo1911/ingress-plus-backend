package iitc

import (
	"github.com/pocketbase/pocketbase"
	"github.com/spf13/cobra"
)

// NewSetupCommand creates the store collections on a pb_data that doesn't
// have them yet (go run . iitc-setup). Meant for local development; in
// production the collections are created by hand from docs/iitc-plugins.md.
func NewSetupCommand(app *pocketbase.PocketBase) *cobra.Command {
	return &cobra.Command{
		Use:   "iitc-setup",
		Short: "Create the IITC plugin store collections if they are missing",
		Run: func(cmd *cobra.Command, args []string) {
			if err := CreateCollections(app); err != nil {
				cmd.PrintErrln("error:", err)
				return
			}
			cmd.Println("IITC plugin store collections are in place.")
		},
	}
}
