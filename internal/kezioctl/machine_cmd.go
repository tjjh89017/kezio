/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kezioctl

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// newMachineCmd builds the `kezioctl machine` command group.
func newMachineCmd(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "Manage kezio Machines",
	}
	cmd.AddCommand(newMachineEnrollCmd(flags))
	cmd.AddCommand(newMachineSetDiskCmd(flags))
	return cmd
}

func newMachineEnrollCmd(flags *globalFlags) *cobra.Command {
	var (
		bmcAddress      string
		bmcSecret       string
		bootMAC         string
		subnetName      string
		subnetNamespace string
	)

	cmd := &cobra.Command{
		Use:   "enroll <name>",
		Short: "Create a Machine",
		Long: `machine enroll creates the Machine CR from the given facts. It performs
no admission checks of its own: the Machine webhook rejects a --subnet
naming a Subnet with no boot half (bootdServerIP/bootdNetworkRef/dhcp), a
--bmc-address whose scheme has no registered driver, and a few other
rules - this command surfaces whatever the API server rejects it with
rather than duplicating any of it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, kubeconfigNamespace, err := NewClient(flags.kubeconfig)
			if err != nil {
				return err
			}
			namespace := flags.resolveNamespace(kubeconfigNamespace)

			machine, err := MachineEnroll(cmd.Context(), c, MachineEnrollOptions{
				Name:                 args[0],
				Namespace:            namespace,
				BMCAddress:           bmcAddress,
				BMCCredentialsSecret: bmcSecret,
				BootMACAddress:       bootMAC,
				SubnetName:           subnetName,
				SubnetNamespace:      subnetNamespace,
			})
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "machine.kezio.kojuro.date/%s created in namespace %q\n",
				machine.Name, machine.Namespace)
			return nil
		},
	}

	cmd.Flags().StringVar(&bmcAddress, "bmc-address", "", "BMC endpoint URL, e.g. redfish://host/... (required)")
	_ = cmd.MarkFlagRequired("bmc-address")
	cmd.Flags().StringVar(&bmcSecret, "bmc-credentials-secret", "", "name of the Secret holding the BMC username and password (required)")
	_ = cmd.MarkFlagRequired("bmc-credentials-secret")
	cmd.Flags().StringVar(&bootMAC, "boot-mac", "", "MAC address of the NIC this machine network boots from (normally discovered by inspection)")
	cmd.Flags().StringVar(&subnetName, "subnet", "", "name of the Subnet this machine network boots through (required)")
	_ = cmd.MarkFlagRequired("subnet")
	cmd.Flags().StringVar(&subnetNamespace, "subnet-namespace", "", "namespace of --subnet, if different from this Machine's own")

	return cmd
}

// targetDiskFlagNames lists every flag newMachineSetDiskCmd registers, so
// its RunE can tell "no hint given" (an error - the command would be a
// no-op) apart from "a hint given with its type's zero value" (int/bool
// flags whose zero value is a real, meaningful hint has no zero value here
// since MinSizeGigabytes/MaxSizeGigabytes/SlotNumber all require >= 1 or
// are otherwise never legitimately 0).
var targetDiskFlagNames = []string{
	"device-name", "serial", "wwn", "model", "vendor",
	"min-size-gb", "max-size-gb", "rotational", "pcie-path", "hctl", "slot-number",
}

func newMachineSetDiskCmd(flags *globalFlags) *cobra.Command {
	var (
		deviceName string
		serial     string
		wwn        string
		model      string
		vendor     string
		minSizeGB  int64
		maxSizeGB  int64
		rotational string
		pciePath   string
		hctl       string
		slotNumber int32
	)

	cmd := &cobra.Command{
		Use:   "set-disk <name>",
		Short: "Set a Machine's target-disk hint",
		Long: `machine set-disk replaces spec.targetDisk with the given hints. All
given hints must match the same disk (logical AND); the controller
matches them against the agent-reported disk inventory at deploy time and
requires exactly one match. At least one hint flag is required.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !anyFlagChanged(cmd, targetDiskFlagNames) {
				return fmt.Errorf("at least one disk hint flag is required (see --help)")
			}

			hints := keziov1alpha3.TargetDiskHints{
				DeviceName:   deviceName,
				SerialNumber: serial,
				WWN:          wwn,
				Model:        model,
				Vendor:       vendor,
				PciePath:     pciePath,
				HCTL:         hctl,
			}
			if cmd.Flags().Changed("min-size-gb") {
				hints.MinSizeGigabytes = &minSizeGB
			}
			if cmd.Flags().Changed("max-size-gb") {
				hints.MaxSizeGigabytes = &maxSizeGB
			}
			if cmd.Flags().Changed("slot-number") {
				hints.SlotNumber = &slotNumber
			}
			if rotational != "" {
				val, err := strconv.ParseBool(rotational)
				if err != nil {
					return fmt.Errorf("--rotational must be \"true\" or \"false\", got %q", rotational)
				}
				hints.Rotational = &val
			}

			c, kubeconfigNamespace, err := NewClient(flags.kubeconfig)
			if err != nil {
				return err
			}
			namespace := flags.resolveNamespace(kubeconfigNamespace)

			if err := MachineSetDisk(cmd.Context(), c, MachineSetDiskOptions{
				Name:       args[0],
				Namespace:  namespace,
				TargetDisk: hints,
			}); err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "machine.kezio.kojuro.date/%s target disk updated\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVar(&deviceName, "device-name", "", "kernel device path, e.g. /dev/nvme0n1 (least stable hint)")
	cmd.Flags().StringVar(&serial, "serial", "", "disk serial number")
	cmd.Flags().StringVar(&wwn, "wwn", "", "disk World Wide Name")
	cmd.Flags().StringVar(&model, "model", "", "disk model string")
	cmd.Flags().StringVar(&vendor, "vendor", "", "disk vendor string")
	cmd.Flags().Int64Var(&minSizeGB, "min-size-gb", 0, "reject disks smaller than this size, in gigabytes")
	cmd.Flags().Int64Var(&maxSizeGB, "max-size-gb", 0, "reject disks larger than this size, in gigabytes")
	cmd.Flags().StringVar(&rotational, "rotational", "", `match a spinning disk ("true") or solid-state ("false")`)
	cmd.Flags().StringVar(&pciePath, "pcie-path", "", "disk PCIe address")
	cmd.Flags().StringVar(&hctl, "hctl", "", "disk SCSI host:channel:target:lun address, e.g. 0:0:0:0")
	cmd.Flags().Int32Var(&slotNumber, "slot-number", 0, "disk NVMe namespace or PCIe slot number")

	return cmd
}

// anyFlagChanged reports whether the user set any of the named flags on
// cmd.
func anyFlagChanged(cmd *cobra.Command, names []string) bool {
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}
