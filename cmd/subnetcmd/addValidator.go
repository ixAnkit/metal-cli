// Copyright (C) 2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
package subnetcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ava-labs/avalanche-cli/pkg/constants"
	"github.com/ava-labs/avalanche-cli/pkg/models"
	"github.com/ava-labs/avalanche-cli/pkg/subnet"
	"github.com/ava-labs/avalanche-cli/pkg/ux"
	"github.com/ava-labs/avalanchego/ids"
	avago_constants "github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/spf13/cobra"
)

var (
	nodeIDStr    string
	weight       int64
	startTimeStr string
	duration     time.Duration

	errNoSubnetID    = errors.New("failed to find the subnet ID for this subnet, has it been deployed/created on this network?")
	startTimeDefault = time.Now().Add(constants.StakingStartLeadTime)
)

// avalanche subnet deploy
func newAddValidatorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "addValidator [subnetName]",
		Short: "Allow a validator to validate your subnet",
		Long: `The subnet addValidator command whitelists a primary network validator to
validate the provided deployed subnet.

To add the validator to the subnet's allow list, you first need to provide
the subnetName and the validator's unique NodeID. The command then prompts
for the validation start time, duration and stake weight. These values can
all be collected with flags instead of prompts.

This command currently only works on subnets deployed to the Fuji testnet.`,
		SilenceUsage: true,
		RunE:         addValidator,
		Args:         cobra.ExactArgs(1),
	}
	cmd.Flags().StringVarP(&keyName, "key", "k", "", "select the key to use")
	cmd.Flags().StringVar(&nodeIDStr, "nodeID", "", "set the NodeID of the validator to add")
	cmd.Flags().Int64Var(&weight, "weight", 0, "set the staking weight of the validator to add")
	cmd.Flags().StringVar(&startTimeStr, "start-time", "", "UTC start time when this validator starts validating, in 'YYYY-MM-DD HH:MM:SS' format")
	cmd.Flags().DurationVar(&duration, "staking-period", 0, "how long this validator will be staking")
	return cmd
}

func addValidator(cmd *cobra.Command, args []string) error {
	var (
		nodeID ids.NodeID
		weight int64
		start  time.Time
		err    error
	)

	if keyName == "" {
		keyName, err = captureKeyName()
		if err != nil {
			return err
		}
	}

	var network models.Network
	networkStr, err := app.Prompt.CaptureList(
		"Choose a network to deploy on. This command only supports Fuji currently.",
		[]string{models.Fuji.String(), models.Mainnet.String() + " (coming soon)"},
	)
	if err != nil {
		return err
	}
	network = models.NetworkFromString(networkStr)

	chains, err := validateSubnetNameAndGetChains(args)
	if err != nil {
		return err
	}
	subnetName := chains[0]
	sc, err := app.LoadSidecar(subnetName)
	if err != nil {
		return err
	}

	subnetID := sc.Networks[network.String()].SubnetID
	if subnetID == ids.Empty {
		return errNoSubnetID
	}

	if nodeIDStr == "" {
		nodeID, err = promptNodeID()
		if err != nil {
			return err
		}
	} else {
		nodeID, err = ids.NodeIDFromString(nodeIDStr)
		if err != nil {
			return err
		}
	}

	if weight == 0 {
		weight, err = promptWeight()
		if err != nil {
			return err
		}
	} else if weight < constants.MinStakeWeight || weight > constants.MaxStakeWeight {
		return fmt.Errorf("illegal weight, must be between 1 and 100 inclusive: %d", weight)
	}

	start, duration, err = getTimeParameters(network, nodeID)
	if err != nil {
		return err
	}

	ux.Logger.PrintToUser("NodeID: %s", nodeID.String())
	ux.Logger.PrintToUser("Network: %s", network.String())
	ux.Logger.PrintToUser("Start time: %s", start.Format(constants.TimeParseLayout))
	ux.Logger.PrintToUser("End time: %s", start.Add(duration).Format(constants.TimeParseLayout))
	ux.Logger.PrintToUser("Weight: %d", weight)
	ux.Logger.PrintToUser("Inputs complete, issuing transaction to add the provided validator information...")
	deployer := subnet.NewPublicDeployer(app, app.GetKeyPath(keyName), network)
	return deployer.AddValidator(subnetID, nodeID, uint64(weight), start, duration)
}

func promptDuration(start time.Time) (time.Duration, error) {
	for {
		txt := "How long should this validator be validating? Enter a duration, e.g. 8760h"
		d, err := app.Prompt.CaptureDuration(txt)
		if err != nil {
			return 0, err
		}
		end := start.Add(d)
		confirm := fmt.Sprintf("Your validator will finish staking by %s", end.Format(constants.TimeParseLayout))
		yes, err := app.Prompt.CaptureYesNo(confirm)
		if err != nil {
			return 0, err
		}
		if yes {
			return d, nil
		}
	}
}

func getMaxValidationTime(network models.Network, nodeID ids.NodeID, startTime time.Time) (time.Duration, error) {
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, constants.RequestTimeout)

	uri := constants.MainnetAPIEndpoint
	if network == models.Fuji {
		uri = constants.FujiAPIEndpoint
	}

	platformCli := platformvm.NewClient(uri)
	vs, err := platformCli.GetCurrentValidators(ctx, avago_constants.PrimaryNetworkID, nil)
	cancel()
	if err != nil {
		return 0, err
	}
	for _, v := range vs {
		if v.NodeID == nodeID {
			return time.Unix(int64(v.EndTime), 0).Sub(startTime), nil
		}
	}
	return 0, errors.New("nodeID not found in validator set: " + nodeID.String())
}

func getTimeParameters(network models.Network, nodeID ids.NodeID) (time.Time, time.Duration, error) {
	var (
		start time.Time
		err   error
	)

	const (
		defaultStartOption    = "Start in five minutes"
		defaultDurationOption = "Until primary network validator expires"
		custom                = "Custom"
	)

	if startTimeStr == "" {
		ux.Logger.PrintToUser("When should your validator start validating?\n" +
			"If you validator is not ready by this time, subnet downtime can occur.")

		startTimeOptions := []string{defaultStartOption, custom}
		startTimeOption, err := app.Prompt.CaptureList("Start time", startTimeOptions)
		if err != nil {
			return time.Time{}, 0, err
		}

		switch startTimeOption {
		case defaultStartOption:
			start = startTimeDefault
		default:
			start, err = promptStart()
			if err != nil {
				return time.Time{}, 0, err
			}
		}
	} else {
		start, err = time.Parse(constants.TimeParseLayout, startTimeStr)
		if err != nil {
			return time.Time{}, 0, err
		}
		if start.Before(time.Now().Add(constants.StakingMinimumLeadTime)) {
			return time.Time{}, 0, fmt.Errorf("time should be at least %s in the future ", constants.StakingMinimumLeadTime)
		}
	}

	if duration == 0 {
		msg := "How long should your validator validate for?"
		durationOptions := []string{defaultDurationOption, custom}
		durationOption, err := app.Prompt.CaptureList(msg, durationOptions)
		if err != nil {
			return time.Time{}, 0, err
		}

		switch durationOption {
		case defaultDurationOption:
			duration, err = getMaxValidationTime(network, nodeID, start)
			if err != nil {
				return time.Time{}, 0, err
			}
		default:
			duration, err = promptDuration(start)
			if err != nil {
				return time.Time{}, 0, err
			}
		}
	}
	return start, duration, nil
}

func promptStart() (time.Time, error) {
	txt := "When should the validator start validating? Enter a UTC datetime in 'YYYY-MM-DD HH:MM:SS' format"
	return app.Prompt.CaptureDate(txt)
}

func promptNodeID() (ids.NodeID, error) {
	txt := "What is the NodeID of the validator you'd like to whitelist?"
	return app.Prompt.CaptureNodeID(txt)
}

func promptWeight() (int64, error) {
	defaultWeight := fmt.Sprintf("Default (%d)", constants.DefaultWeight)
	txt := "What stake weight would you like to assign to the validator?"
	weightOptions := []string{defaultWeight, "Custom"}

	weightOption, err := app.Prompt.CaptureList(txt, weightOptions)
	if err != nil {
		return 0, err
	}

	switch weightOption {
	case defaultWeight:
		return constants.DefaultWeight, nil
	default:
		return app.Prompt.CaptureWeight(txt)
	}
}

func captureKeyName() (string, error) {
	files, err := os.ReadDir(app.GetKeyDir())
	if err != nil {
		return "", err
	}

	keys := make([]string, len(files))

	for i, f := range files {
		if strings.HasSuffix(f.Name(), constants.KeySuffix) {
			keys[i] = strings.TrimSuffix(f.Name(), constants.KeySuffix)
		}
	}

	keyName, err = app.Prompt.CaptureList("Which private key should be used to issue the transaction?", keys)
	if err != nil {
		return "", err
	}

	return keyName, nil
}
