package main

import (
	"fmt"
	"io"
	l "log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/pkg/term/termios"
	"golang.org/x/sys/unix"
)

var log *l.Logger

// https://developer.apple.com/documentation/virtualization/running_linux_in_a_virtual_machine?language=objc#:~:text=Configure%20the%20Serial%20Port%20Device%20for%20Standard%20In%20and%20Out
func setRawMode(f *os.File) {
	var attr unix.Termios

	// Get settings for terminal
	termios.Tcgetattr(f.Fd(), &attr)

	// Put stdin into raw mode, disabling local echo, input canonicalization,
	// and CR-NL mapping.
	attr.Iflag &^= syscall.ICRNL
	attr.Lflag &^= syscall.ICANON | syscall.ECHO

	// Set minimum characters when reading = 1 char
	attr.Cc[syscall.VMIN] = 1

	// set timeout when reading as non-canonical mode
	attr.Cc[syscall.VTIME] = 0

	// reflects the changed settings
	termios.Tcsetattr(f.Fd(), termios.TCSANOW, &attr)
}

// createVMConfig creates a new VirtualMachineConfiguration with the provided parameters.
func createVMConfig(vmlinuz, initrd, diskPath string, virtualStdinWriter *os.File, kernelCommandLineArguments []string) (*vz.VirtualMachineConfiguration, error) {
	bootLoader, err := vz.NewLinuxBootLoader(
		vmlinuz,
		vz.WithCommandLine(strings.Join(kernelCommandLineArguments, " ")),
		vz.WithInitrd(initrd),
	)
	if err != nil {
		return nil, fmt.Errorf("bootloader creation failed: %w", err)
	}

	config, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		1,                // numCPUs
		2*1024*1024*1024, // memorySize
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual machine configuration: %w", err)
	}

	// Create a pipe for virtual stdin
	virtualStdin, virtualStdinWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("Failed to create virtual stdin pipe: %w", err)
	}

	// Use the read end of the pipe as virtual stdin
	serialPortAttachment, err := vz.NewFileHandleSerialPortAttachment(virtualStdin, os.Stdout)
	if err != nil {
		return nil, fmt.Errorf("Serial port attachment creation failed: %w", err)
	}

	// console
	consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialPortAttachment)
	if err != nil {
		return nil, fmt.Errorf("Failed to create serial configuration: %w", err)
	}
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{
		consoleConfig,
	})

	// network
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("NAT network device creation failed: %w", err)
	}
	networkConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	if err != nil {
		return nil, fmt.Errorf("Creation of the networking configuration failed: %w", err)
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkConfig,
	})
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, fmt.Errorf("Random MAC address creation failed: %w", err)
	}
	networkConfig.SetMACAddress(mac)

	// entropy
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("Entropy device creation failed: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{
		entropyConfig,
	})

	diskImageAttachment, err := vz.NewDiskImageStorageDeviceAttachment(
		diskPath,
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("Disk image attachment creation failed: %w", err)
	}
	storageDeviceConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskImageAttachment)
	if err != nil {
		return nil, fmt.Errorf("Block device creation failed: %w", err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{
		storageDeviceConfig,
	})

	// traditional memory balloon device which allows for managing guest memory. (optional)
	// Note this is not supported for snapshotting
	// memoryBalloonDevice, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	// if err != nil {
	// 	log.Fatalf("Balloon device creation failed: %s", err)
	// }
	// config.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{
	// 	memoryBalloonDevice,
	// })

	// socket device (optional)
	vsockDevice, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("virtio-vsock device creation failed: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{
		vsockDevice,
	})

	return config, nil
}

func main() {
	file, err := os.Create("./log.log")
	if err != nil {
		panic(err)
	}
	defer file.Close()
	log = l.New(file, "", l.LstdFlags)

	kernelCommandLineArguments := []string{
		// Use the first virtio console device as system console.
		"console=hvc0",
		// Stop in the initial ramdisk before attempting to transition to
		// the root file system.
		"root=/dev/vda",
	}

	vmlinuz := os.Getenv("VMLINUZ_PATH")
	initrd := os.Getenv("INITRD_PATH")
	diskPath := os.Getenv("DISKIMG_PATH")

	log.Println("vmlinuz:", vmlinuz)
	log.Println("initrd:", initrd)
	log.Println("diskPath:", diskPath)

	virtualStdinWriter, err := os.Create("/tmp/virtual-stdin")
	if err != nil {
		log.Fatalf("failed to create virtual stdin writer: %v", err)
	}
	defer virtualStdinWriter.Close()

	config, err := createVMConfig(vmlinuz, initrd, diskPath, virtualStdinWriter, kernelCommandLineArguments)
	if err != nil {
		log.Fatalf("failed to create VM config: %v", err)
	}

	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		log.Fatalf("failed to create VM: %v", err)
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM)

	// Start a goroutine to write to the virtual stdin
	go func() {
		// Wait for 1 second before writing to virtual stdin
		time.Sleep(1 * time.Second)

		log.Println("Logging in via virtual stdin")

		_, err := io.WriteString(virtualStdinWriter, "root\n")
		if err != nil {
			log.Printf("Failed to write to virtual stdin: %s", err)
		}

		// Wait 100ms
		time.Sleep(100 * time.Millisecond)

		// Send password
		_, err = io.WriteString(virtualStdinWriter, "passwd\n")
		if err != nil {
			log.Printf("Failed to write to virtual stdin: %s", err)
		}

		// Probe what shell it is
		_, err = io.WriteString(virtualStdinWriter, "echo $SHELL\n")
		if err != nil {
			log.Printf("Failed to write to virtual stdin: %s", err)
		}

		// Write the generic shell script
		_, err = io.WriteString(virtualStdinWriter, `
count=0
while true; do
    echo "hello $count"
    count=$((count + 1))
    sleep 1
done
`)
		if err != nil {
			log.Printf("Failed to write to virtual stdin: %s", err)
		}

		time.Sleep(2000 * time.Millisecond)

		// Check if we can pause
		if !vm.CanPause() {
			log.Println("VM cannot pause")
			os.Exit(1)
			_, err := vm.RequestStop()
			if err != nil {
				log.Println("request stop error:", err)
				os.Exit(1)
			}
		}

		log.Println("Request pause VM")
		if err := vm.Pause(); err != nil {
			log.Println("request pause error:", err)
			os.Exit(1)
		}

		// Delete the save file first
		if err := os.Remove("savestate"); err != nil {
			log.Println("remove save state error:", err)
		}

		if err := vm.SaveMachineStateToPath("savestate"); err != nil {
			log.Println("save state with error", err)
			os.Exit(1)
		}

		log.Println("VM paused")

		// Now just kill the VM
		if err := vm.Stop(); err != nil {
			log.Println("stop error:", err)
			os.Exit(1)
		}

		// Wait for 1 second
		time.Sleep(1 * time.Second)

		// Now let's make a whole new VM with the same config
		newVM, err := vz.NewVirtualMachine(config)
		if err != nil {
			log.Println("new VM creation failed:", err)
			os.Exit(1)
		}

		if err := newVM.RestoreMachineStateFromURL("savestate"); err != nil {
			log.Println("restore state error:", err)
			os.Exit(1)
		}

		fmt.Println("VM restored")

		// Resume the VM
		if err := newVM.Resume(); err != nil {
			log.Println("resume error:", err)
			os.Exit(1)
		}

		// Now wait 5 seconds
		time.Sleep(5 * time.Second)

		// Kill the VM
		if err := newVM.Stop(); err != nil {
			log.Println("stop error:", err)
			os.Exit(1)
		}

		log.Println("VM successfully started and stopped")

		// Stop the VM
		virtualStdinWriter.Close()
		os.Exit(0)

	}()

	errCh := make(chan error, 1)

	for {
		select {
		case <-signalCh:
			log.Println("recieved signal to stop")
			result, err := vm.RequestStop()
			if err != nil {
				log.Println("request stop error:", err)
			}
			log.Println("recieved signal", result)
		case newState := <-vm.StateChangedNotify():
			if newState == vz.VirtualMachineStateRunning {
				log.Println("start VM is running")
			}
			if newState == vz.VirtualMachineStateStopped {
				log.Println("stopped successfully")
			}
		case err := <-errCh:
			log.Println("in start:", err)
		}
	}

	// if err := vm.Resume(); err != nil {
	// 	log.Println("in resume:", err)
	// }
}
