package captive

import "syscall"

// stopSignal asks dnsmasq to exit.
const stopSignal = syscall.SIGTERM
