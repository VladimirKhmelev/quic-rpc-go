// Package netns builds the network testbed (NET-1…NET-3): three network
// namespaces, client — router — server, joined by veth pairs, with the
// impairments of a profile on the two router interfaces, one per direction.
//
// The testbed needs no root on the host. Enter starts the program again in
// new user, mount and network namespaces, where it is root; the testbed
// built there with Stand disappears when the program exits, so a crash
// leaves nothing behind. Commands run in the namespaces with ip netns exec,
// see Command.
//
// The commands that build and change the testbed are plain data
// (UpCommands, ApplyCommands), so they can be printed and tested without a
// network.
package netns
