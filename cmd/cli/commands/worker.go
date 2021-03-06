package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sonm-io/core/insonmnia/auth"
	"github.com/sonm-io/core/proto"
	"github.com/sonm-io/core/util"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"gopkg.in/yaml.v2"
)

var (
	workerAddressFlag string
	worker            sonm.WorkerManagementClient
	workerAddr        string
	workerCtx         context.Context
	workerConn        *grpc.ClientConn
	workerCancel      context.CancelFunc
)

func workerPreRunE(cmd *cobra.Command, args []string) error {
	if err := loadKeyStoreWrapper(cmd, args); err != nil {
		return err
	}

	workerCtx, workerCancel = newTimeoutContext()
	workerAddr = cfg.WorkerAddr
	if len(workerAddressFlag) != 0 {
		workerAddr = workerAddressFlag
	}
	if len(workerAddr) == 0 {
		key, err := getDefaultKey()
		if err != nil {
			return err
		}
		workerAddr = crypto.PubkeyToAddress(key.PublicKey).Hex()
	}

	md := metadata.MD{
		util.WorkerAddressHeader: []string{workerAddr},
	}
	workerCtx = metadata.NewOutgoingContext(workerCtx, md)

	conn, err := newClientConn(workerCtx)
	if err != nil {
		return err
	}

	workerConn = conn

	worker, err = newWorkerManagementClient(workerCtx)
	if err != nil {
		return fmt.Errorf("cannot create client connection: %v", err)
	}

	return nil
}

func workerPostRun(_ *cobra.Command, _ []string) {
	if workerCancel != nil {
		workerCancel()
	}
}

func init() {
	workerMgmtCmd.PersistentFlags().StringVar(&workerAddressFlag, "worker-address", "", "Use specified worker address instead of configured value")
	workerMgmtCmd.AddCommand(
		workerStatusCmd,
		askPlansRootCmd,
		benchmarkRootCmd,
		workerTasksCmd,
		workerDevicesCmd,
		workerFreeDevicesCmd,
		workerSwitchCmd,
		workerCurrentCmd,
		workerScheduleMaintenanceCmd,
		workerNextMaintenanceCmd,
		workerDebugStateCmd,
		workerLogs,
		workerAddCapabilityCmd,
		workerRemoveCapabilityCmd,
		workerMetricsCmd,
		workerConfigCmd,
	)
}

var workerMgmtCmd = &cobra.Command{
	Use:               "worker",
	Short:             "Worker management",
	PersistentPreRunE: workerPreRunE,
	PersistentPostRun: workerPostRun,
}

var workerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show worker status",
	RunE: func(cmd *cobra.Command, _ []string) error {
		status, err := worker.Status(workerCtx, &sonm.Empty{})
		if err != nil {
			return fmt.Errorf("cannot get worker status: %v", err)
		}

		printWorkerStatus(cmd, status)
		return nil
	},
}

var workerSwitchCmd = &cobra.Command{
	Use:   "switch <eth_addr>",
	Short: "Switch current worker to specified addr",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, err := auth.ParseAddr(args[0])
		if err != nil {
			return fmt.Errorf("invalid address specified: %v", err)
		}

		if _, err := addr.ETH(); err != nil {
			return fmt.Errorf("could not parse eth component of the auth addr - it's malformed or missing")
		}

		cfg.WorkerAddr = addr.String()
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("failed to save worker address: %v", err)
		}

		showOk(cmd)
		return nil
	},
}

var workerScheduleMaintenanceCmd = &cobra.Command{
	Use:   "maintenance <at or after>",
	Short: "Schedule worker maintanance",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var timePoint time.Time
		timeData := []byte(args[0])
		if err := timePoint.UnmarshalText(timeData); err != nil {
			duration, err := time.ParseDuration(args[0])
			if err != nil {
				return fmt.Errorf("invalid time point or duration specified: %v", err)
			}

			timePoint = time.Now()
			timePoint = timePoint.Add(duration)
		}

		if _, err := worker.ScheduleMaintenance(workerCtx, &sonm.Timestamp{Seconds: timePoint.Unix()}); err != nil {
			return fmt.Errorf("failed to schedule maintenance: %v", err)
		}

		showOk(cmd)
		return nil
	},
}

var workerNextMaintenanceCmd = &cobra.Command{
	Use:   "next-maintenance",
	Short: "Print next scheduled maintenance",
	RunE: func(cmd *cobra.Command, args []string) error {
		next, err := worker.NextMaintenance(workerCtx, &sonm.Empty{})
		if err != nil {
			return fmt.Errorf("failed to get next maintenance: %v", err)
		}

		// todo: proper printer
		if isSimpleFormat() {
			cmd.Println(next.Unix().String())
		} else {
			showJSON(cmd, next.Unix())
		}

		return nil
	},
}

var workerCurrentCmd = &cobra.Command{
	Use:   "current",
	Short: "Show current worker's addr",
	RunE: func(cmd *cobra.Command, args []string) error {
		type Result struct {
			Address     string `json:"address,omitempty"`
			Error       error  `json:"error,omitempty"`
			Description string `json:"description,omitempty"`
		}

		result := func() Result {
			result := Result{}
			if len(cfg.WorkerAddr) == 0 {
				key, err := getDefaultKey()
				if err != nil {
					result.Error = err
					return result
				}

				result.Description = "current worker is not set, using cli's addr"
				result.Address = crypto.PubkeyToAddress(key.PublicKey).Hex()
				return result
			}
			addr, err := auth.ParseAddr(cfg.WorkerAddr)
			if err != nil {
				result.Error = err
				result.Description = fmt.Sprintf("current worker(%s) is invalid", cfg.WorkerAddr)
				return result
			}
			if _, err := addr.ETH(); err != nil {
				result.Error = errors.New("could not parse eth component of the auth addr - it's malformed or missing")
				result.Description = fmt.Sprintf("current worker(%s) is invalid", cfg.WorkerAddr)
				return result
			}
			result.Description = "current worker is"
			result.Address = addr.String()
			return result
		}()

		if result.Error != nil {
			return fmt.Errorf("%s: %v", result.Description, result.Error)
		}

		if isSimpleFormat() {
			cmd.Printf("%s %s\n", result.Description, result.Address)
		} else {
			showJSON(cmd, result)
		}
		return nil
	},
}

var workerDebugStateCmd = &cobra.Command{
	Use:   "debug-state",
	Short: "Provide some useful worker debugging info",
	RunE: func(cmd *cobra.Command, args []string) error {
		reply, err := worker.DebugState(workerCtx, &sonm.Empty{})
		if err != nil {
			return fmt.Errorf("failed to get debug state: %v", err)
		}

		if isSimpleFormat() {
			data, err := yaml.Marshal(reply)
			if err != nil {
				return fmt.Errorf("failed to marshal state: %v", err)
			}

			cmd.Printf("%s\r\n", string(data))
		} else {
			showJSON(cmd, reply)
		}

		return nil
	},
}

var workerLogs = &cobra.Command{
	Use:   "logs",
	Short: "Subscribe for worker's logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := metadata.NewOutgoingContext(context.Background(), metadata.MD{
			util.WorkerAddressHeader: []string{workerAddr},
		})

		cc, err := newClientConn(ctx)
		if err != nil {
			return err
		}

		steam, err := sonm.NewInspectClient(cc).WatchLogs(ctx, &sonm.InspectWatchLogsRequest{})
		if err != nil {
			return err
		}

		for {
			message, err := steam.Recv()
			if err != nil {
				if err == io.EOF {
					return nil
				}

				return err
			}

			cmd.Printf("%s", message.GetMessage())
		}
	},
}

var workerAddCapabilityCmd = &cobra.Command{
	Use:   "add-capability ETH_ADDR CAPABILITY TTL",
	Short: "Add the given worker-specific capability to another user",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, err := sonm.NewEthAddressFromHex(args[0])
		if err != nil {
			return err
		}

		scope, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return err
		}

		ttl, err := strconv.ParseUint(args[2], 10, 32)
		if err != nil {
			return err
		}

		request := &sonm.WorkerAddCapabilityRequest{
			Subject: sonm.NewEthAddress(addr.Unwrap()),
			Scope:   sonm.CapabilityScope(scope),
			Ttl:     uint32(ttl),
		}

		if _, err := worker.AddCapability(workerCtx, request); err != nil {
			return fmt.Errorf("failed to add capability: %v", err)
		}

		showOk(cmd)

		return nil
	},
}

var workerRemoveCapabilityCmd = &cobra.Command{
	Use:   "remove-capability ETH_ADDR CAPABILITY",
	Short: "Revoke the given worker-specific capability from another user",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, err := sonm.NewEthAddressFromHex(args[0])
		if err != nil {
			return err
		}

		scope, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return err
		}

		request := &sonm.WorkerRemoveCapabilityRequest{
			Subject: sonm.NewEthAddress(addr.Unwrap()),
			Scope:   sonm.CapabilityScope(scope),
		}

		if _, err := worker.RemoveCapability(workerCtx, request); err != nil {
			return fmt.Errorf("failed to remove capability: %v", err)
		}

		showOk(cmd)

		return nil
	},
}

var workerConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Shows Worker's config.",
	RunE: func(cmd *cobra.Command, args []string) error {
		inspect := sonm.NewInspectClient(workerConn)
		request := &sonm.InspectConfigRequest{}
		response, err := inspect.Config(workerCtx, request)
		if err != nil {
			return fmt.Errorf("failed to fetch Worker config: %v", err)
		}

		var target interface{}
		if err := json.Unmarshal(response.GetConfig(), &target); err != nil {
			return err
		}

		if isSimpleFormat() {
			data, _ := yaml.Marshal(&target)
			cmd.Printf("%s\n", data)
		} else {
			showJSON(cmd, target)
		}

		return nil
	},
}
