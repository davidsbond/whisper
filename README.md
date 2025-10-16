# whisper

A gRPC-based gossip protocol

## About

`whisper` is a go package providing a simple gossip protocol mechanism that allows peers within a network to discover
each other and share metadata. It utilises UDP and ECDH for fast and secure convergence. It is heavily inspired by
[HashiCorp Serf](https://github.com/hashicorp/serf) and [Wireguard](https://www.wireguard.com/).

### Joining

A peer joins the network using an initial TCP connection to an existing peer. When joining, the peer provides
information about itself, including its public key and an advertised address used for further UDP/TCP communication. If
accepted into the network, the peer is given the current state of all known peers from the peer it joined.

Peers do not share their private keys and the ability to join the network is handled at the TCP layer using TLS.

### Sharing State

Once joined, the new peer regularly shares its state (including other peers it is aware of) with other random peers over
UDP. Each packet contains the identifier of the peer sending the data & encrypted information on a single peer in the
network. When sending a packet, the sender encrypts it using a shared secret derived from the sender's private key and
the receiver's public key. When a peer receives each packet, it looks up the sender's public key & decrypts the packet
using a shared secret derived from the sender's public key & receivers private key.

If the provided peer information is newer (indicated by a delta), the receiver's local state is updated. If the peer
information is older, the peer sends its own encrypted UDP packet containing the state for the particular out-of-date
peer information to the sender. This allows the network to converge fairly quickly, as peers notice out-of-date
information and update each other.

Naturally, gossip protocols are quite chatty, which led to the choice of [protocol buffers](https://protobuf.dev/) as
the serialisation method to reduce the required network bandwidth.

#### Peer States

Peers can be in one of 5 states at a given time:

* `Joining` - The peer is currently in the process of joining the network and synchronizing its state via another peer.
* `Joined` - The peer is actively participating in the gossip network (sharing state, checking peer status etc.).
* `Leaving` - The peer is currently in the process of leaving the network, informing a target peer of its intent to
  leave the network.
* `Left` - The peer has left the network and is no longer participating in the gossip network.
* `Gone` - Other peers are unable to communicate with this peer.

### Failure Detection

Failures in peers are detected through regular status checks. Each peer will select a random peer to query for its
current status. If the target peer is found to be unavailable, the checking peer will request that up to 3 other active
peers also perform the status check against the target peer. If none of the selected peers are able to reach the target
peer, the checking peer marks the target peer as "gone". This "gone" peer will no longer be selected for state updates
and status checks.

Using a verification process via other peers allows the network to handle transient issues. A "gone" peer will become
available again once it is able to send its own state to other peers in the network.

### Leaving

Leaving the network works very similarly to joining the network. The leaving peer announces to a randomly selected peer
that it intends to leave. The receiving peer marks the peer as leaving in its own state, which will propagate out to
other peers.

The leaving peer then shuts down its TCP/UDP listeners and exits gracefully.

## Usage

This repository provides two ways of running whisper, the first is as a package called directly within your go
program:

```go
// See the package documentation for all available configuration options.
node := whisper.New(id, options...)

// This blocks until the given context is cancelled or a fatal error occurs, use it in a separate goroutine or
// an error group
node.Run(ctx)

// Wait for the node to be ready. This also blocks, but you'll want to wait for it to return before you try to
// fully use the node for your own purposes. If you specify a join address, this will return once the node has
// joined and synchronised its own state. For a standalone node, it will return fairly instantly.
node.Ready(ctx)
```

The second is as a standalone binary you can grab from the releases page:

```shell
# As a standalone node
whisper start 1

# Or joining an existing network
whisper start 2 --join 123.123.123.123:8000

# You can also then query the network status via the cli
whisper status 0.0.0.0:8000

# {
#   "self": {
#     "id": "2",
#     "address": "0.0.0.0:8001",
#     "publicKey": "RSxbJaMS5bBkcYjodsqORLjtVcWwthshTgb3+X9AaXI=",
#     "delta": "1760558837781219829",
#     "status": "PEER_STATUS_JOINED"
#   },
#   "peers": [
#     {
#       "id": "1",
#       "address": "0.0.0.0:8000",
#       "publicKey": "gyFephOI2gTZ2bQvpyBtfR5B9HmJF6oesvT4hGni/hU=",
#       "delta": "1760558840695516019",
#       "status": "PEER_STATUS_JOINED"
#     },
#     {
#       "id": "3",
#       "address": "0.0.0.0:8002",
#       "publicKey": "LcJVlapUKS2YWp2jT/M/rTd4jzMAXXfcxi4igqX7MxI=",
#       "delta": "1760558840305826881",
#       "status": "PEER_STATUS_JOINED"
#     }
#   ]
# }

# Or manually perform connectivity tests via peers
whisper check 3 0.0.0.0:8000
```
