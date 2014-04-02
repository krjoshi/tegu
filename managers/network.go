// vi: sw=4 ts=4:

/*

	Mnemonic:	network
	Abstract:	Manages everything associated with a network. This module contains a 
				goroutine which should be invoked from the tegu main and is responsible
				for managing the network graph and responding to requests for information about
				the network graph. As a part of the collection of functions here, there is also 
				a tickler which causes the network graph to be rebuilt on a regular basis. 

				The network manager goroutine listens to a channel for requests such as finding
				and reserving a path between two hosts, and generating a json representation 
				of the network graph for outside consumption.

				TODO: need to look at purging links/vlinks so they don't bloat if the network changes 

	Date:		24 November 2013
	Author:		E. Scott Daniels

	Mods:		19 Jan 2014 - Added support for host-any reservations.
				11 Feb 2014 - Support for queues on links rather than just blanket obligations per link. 
				21 Mar 2014 - Added noop support to allow main to hold off driving checkpoint 
							loading until after the driver here has been entered and thus we've built
							the first graph.
*/

package managers

import (
	//"bufio"
	//"encoding/json"
	//"flag"
	"fmt"
	//"io/ioutil"
	//"html"
	//"net/http"
	"os"
	"strings"
	//"time"

	"forge.research.att.com/gopkgs/bleater"
	"forge.research.att.com/gopkgs/clike"
	"forge.research.att.com/gopkgs/ipc"
	"forge.research.att.com/tegu"
	"forge.research.att.com/tegu/gizmos"
	
)

// --------------------------------------------------------------------------------------

type Network struct {						// defines a network
	switches	map[string]*gizmos.Switch			// symtable of switches
	hosts		map[string]*gizmos.Host			// references to host by either mac, ipv4 or ipv6 'names'
	links		map[string]*gizmos.Link			// table of links allows for update without resetting allotments
	vlinks		map[string]*gizmos.Link			// table of virtual links (links between ports on the same switch)
	vm2ip		map[string]*string			// maps vm names and vm IDs to IP addresses (generated by ostack and sent on channel)
	ip2vm		map[string]*string			// reverse -- makes generating complete host listings faster
}

// ------------ private -------------------------------------------------------------------------------------

/*
	constructor
*/
func mk_network( mk_links bool ) ( n *Network ) {
	n = &Network { }
	n.switches = make( map[string]*gizmos.Switch, 20 )		// initial sizes aren't limits, but might help save space
	n.hosts = make( map[string]*gizmos.Host, 2048 )

	if mk_links {
		n.links = make( map[string]*gizmos.Link, 2048 )	// must maintain a list of links so when we rebuild we preserve obligations
		n.vlinks = make( map[string]*gizmos.Link, 2048 )
	}

	return
}

/*
	Build the ip2vm map from the vm2ip map which is a map of IP addresses to what we hope is the VM
	name.  The vm2ip map contains both VM IDs and the names the user assigned to the VM. We'll guess
	at getting the name from the map.

	TODO: we need to change vm2ip to be struct with two maps rather than putting IDs and names into the same map
*/
func (n *Network) build_ip2vm( ) ( i2v map[string]*string ) {

	i2v = make( map[string]*string )
	
	for k, v := range n.vm2ip {
		if len( k ) < 36 || i2v[*v] == nil {		// IDs seem to be 36, but we'll save something regardless and miss if user went wild with long name and we hit it second
			dup_str := k							// 'dup' the string so we don't reference the string associated with the other map
			i2v[*v] = &dup_str
			net_sheep.Baa( 3, "build_ip2vm %s --> %s %d", *v, i2v[*v], len( k ) )
		}
	}

	net_sheep.Baa( 3, "built ip2vm map: %d entires", len( i2v ) )
	return
}


/*
	Accepts a map of queue data information (swid/port,res-id,queue,min,max,pri)
	And adds to that based on the contents of the string if the data isn't already
	listed in the seen map (prevents dups). Insertion into the map begins at idx
	allowing this to be called multiple times to build a single map.

	Returns both the map and the next insertion point. We must return the map
	on the off chance that we had to increase it's size and thus it's new. 

	(primarlly support for gen_queue_map and probably nothing else)
*/
func qlist2map( qmap []string, qlist *string, idx int, seen map[string]int ) ( []string, int ) {

	qdata := strings.Split( *qlist, " " )		// split the list into tokens
	for i := range qdata {
		if idx >= len( qmap ) {
			nqmap := make( []string, len( qmap ) + 128 )
			copy( nqmap, qmap )
		}

		if qdata[i] != "" && seen[qdata[i]] == 0 {
			seen[qdata[i]] = 1
			qmap[idx] = qdata[i]
			idx++
		}
	}

	return qmap, idx
}

/*
	Traverses all known links and generates a switch queue map based on the queues set for 
	the time indicated by the timestamp passed in (ts). 
*/
func (n *Network) gen_queue_map( ts int64 ) ( qmap []string, err error ) {
	var (
		idx 	int = 0
	)

	err = nil									// at the moment we always succeed
	qmap = make( []string, 128 )
	seen := make( map[string]int, 100 )			// prevent dups which occur because of double links

	for _, link := range n.links {					// for each link in the graph
		s := link.Queues2str( ts )
		qmap, idx = qlist2map( qmap, &s, idx, seen )		// add it's set to the map
	}

	for _, link := range n.vlinks {					// and do the same for vlinks
		s := link.Queues2str( ts )
		qmap, idx = qlist2map( qmap, &s, idx, seen )		// add it's set to the map
	}

	qmap = qmap[:idx]
	return
}



/*
	DEPRECATED
	return the maximum value allocated to any link for the given time
func (n *Network) get_max_link_alloc( utime int64 ) ( max int64 ) {
	max = 0

	for _, lp := range n.links {
		if lav := lp.Get_allocation( utime ); lav > max {
			max = lav
		}
	}	

	return
}
*/

/*
	return the ip address associated with the name. The name may indeed be 
	an IP address which we'll look up in the hosts table to verify first. 
	If it's not an ip, then we'll search the vm2ip table for it. 
*/
func (n *Network) name2ip( hname *string ) (ip *string, err error) {
	ip = nil
	err = nil

/*
	if *hname == "any" || *hname == "ext" {
		dup_str := "0.0.0.0"
		ip = &dup_str
		return
	}
*/

	if n.hosts[*hname] != nil {			// we have a host by 'name', then 'name' must be an ip address
		ip = hname
	} else {
		ip = n.vm2ip[*hname]					// it's not an ip, try to translate it as either a VM name or VM ID
		if ip != nil {							// the name translates, see if it's in the known net
			if n.hosts[*ip] == nil {			// ip isn't in floodlight scope, return nil
				err = fmt.Errorf( "host unknown: %s maps to an IP, but IP not known to SDNC: %s", *hname, *ip )
				ip = nil
			}
		} else {
			err = fmt.Errorf( "host unknown: %s could not be mapped to an IP address", *hname )
			//net_sheep.Baa( 1, "unable to map name/ID to an IP: %s", *hname )						// caller should bleat
		}
	}

	return
}

/*
	given two switch names see if we can find an existing link in the src->dest direction
	if lnk is passed in, that is passed through to Mk_link() to cause lnk's obligation to be
	'bound' to the link that is created here. 

	If the link between src-sw and dst-sw is not there, one is created and added to the map.

	We use this to reference the links from the previously created graph so as to preserve obligations.
	(TODO: it would make sense to vet the obligations to ensure that they can still be met should
	a switch depart from the network.)
*/
func (n *Network) find_link( ssw string, dsw string, capacity int64, lnk ...*gizmos.Link ) (l *gizmos.Link) {

	id := fmt.Sprintf( "%s-%s", ssw, dsw )
	l = n.links[id]
	if l != nil {
		if lnk != nil {										// dont assume that the links shared the same allotment previously though they probably do
			l.Set_allotment( lnk[0].Get_allotment( ) )
		}
		return
	}

	net_sheep.Baa( 3, "making link: %s", id )
	if lnk == nil {
		l = gizmos.Mk_link( &ssw, &dsw, capacity );	
	} else {
		l = gizmos.Mk_link( &ssw, &dsw, capacity, lnk[0] );	
	}
	n.links[id] = l
	return
}

/*
	Looks for a virtual link on the switch given between ports 1 and 2.
	Returns the existing link, or makes a new one if this is the first.
	New vlinks are stashed into the vlink hash.
*/
func (n Network) find_vlink( sw string, p1 int, p2 int ) ( l *gizmos.Link ) {
	id := fmt.Sprintf( "%s.%d.%d", sw, p1, p2 )
	l = n.vlinks[id]
	if l == nil {
		l = gizmos.Mk_vlink( &sw, p1, p2, int64( 10 * ONE_GIG ) )
		l.Set_ports( p1, p2 )
		n.vlinks[id] = l
	}

	return
}

/*
	build a new graph of the network
	host is the name/ip:port of the host where floodlight is running
	old-net is the reference net that we'll attempt to find existing links in.
	max_capacity is the generic (default) max capacity for each link
*/
func build( old_net *Network, flhost *string, max_capacity int64 ) (n *Network) {
	var (
		ssw		*gizmos.Switch
		dsw		*gizmos.Switch
		lnk		*gizmos.Link
		ip4		string
		ip6		string
	)

	n = nil
	links := gizmos.FL_links( flhost )					// request the current set of links from floodlight
	if links == nil {
		return
	}
	hlist := gizmos.FL_hosts( flhost )					// get a current host list from floodlight
	if hlist == nil {
		return
	}

	n = mk_network( old_net == nil )			// new network, need links only if it's the first network
	if old_net == nil {
		old_net = n								// prevents an if around every try to find an existing link.
	} else {
		n.links = old_net.links;				// might it be wiser to copy this rather than reference and update the 'live' copy?
		n.vlinks = old_net.vlinks;
	}

	for i := range links {							// parse all links returned from the controller
		ssw = n.switches[links[i].Src_switch]; 
		if ssw == nil {
			ssw = gizmos.Mk_switch( &links[i].Src_switch )
			n.switches[links[i].Src_switch] = ssw
		}

		dsw = n.switches[links[i].Dst_switch]; 
		if dsw == nil {
			dsw = gizmos.Mk_switch( &links[i].Dst_switch )
			n.switches[links[i].Dst_switch] = dsw
		}

		lnk = old_net.find_link( links[i].Src_switch, links[i].Dst_switch, max_capacity )		// omitting the link causes reuse of the link if it existed so that obligations are kept
		lnk.Set_forward( dsw )
		lnk.Set_backward( ssw )
		lnk.Set_port( 1, links[i].Src_port )		// port on src to dest
		lnk.Set_port( 2, links[i].Dst_port )		// port on dest to src
		ssw.Add_link( lnk )

		lnk = old_net.find_link( links[i].Dst_switch, links[i].Src_switch, max_capacity, lnk )	// including the link causes its obligation to be shared in this direction
		lnk.Set_forward( ssw )
		lnk.Set_backward( dsw )
		lnk.Set_port( 1, links[i].Dst_port )		// port on dest to src
		lnk.Set_port( 2, links[i].Src_port )		// port on src to dest
		dsw.Add_link( lnk )
		net_sheep.Baa( 3, "build: addlink: src [%d] %s %s", i, links[i].Src_switch, n.switches[links[i].Src_switch].To_json() )
		net_sheep.Baa( 3, "build: addlink: dst [%d] %s %s", i, links[i].Dst_switch, n.switches[links[i].Dst_switch].To_json() )
	}

	for i := range hlist {							// parse the unpacked json; structs are very dependent on the floodlight output; TODO: change FL_host to return a generic map
		if len( hlist[i].Mac )  > 0  && len( hlist[i].AttachmentPoint ) > 0 {		// switches come back in the list; if there are no attachment points we assume it's a switch & drop
			if len( hlist[i].Ipv4 ) > 0 {
				ip4 = hlist[i].Ipv4[0]; 
			} else {
				ip4 = ""
			}
			if len( hlist[i].Ipv6 ) > 0 {
				ip6 = hlist[i].Ipv6[0]; 
			} else {
				ip6 = ""
			}

			h := gizmos.Mk_host( hlist[i].Mac[0], ip4, ip6 )

			if len( hlist[i].AttachmentPoint ) > 0 {
				for j := 0; j < len( hlist[i].AttachmentPoint ); j++ {
					h.Add_switch( n.switches[hlist[i].AttachmentPoint[j].SwitchDPID], hlist[i].AttachmentPoint[j].Port )
					ssw = n.switches[hlist[i].AttachmentPoint[j].SwitchDPID]
					if ssw != nil {							// it should always be known, but no chances
						ssw.Add_host( &hlist[i].Mac[0], hlist[i].AttachmentPoint[j].Port )	// allows switch to provide has_host() method
						net_sheep.Baa( 3, "saving host %s in switch : %s port: %d", hlist[i].Mac[0], hlist[i].AttachmentPoint[j].SwitchDPID, hlist[i].AttachmentPoint[j].Port )
					}
				}
			}

			n.hosts[hlist[i].Mac[0]] = h			// reference by mac and IP addresses (when there)
			net_sheep.Baa( 2, "build: saving host as mac: %s", hlist[i].Mac[0] )
			if len( hlist[i].Ipv4 ) > 0 && hlist[i].Ipv4[0] != "" {
				net_sheep.Baa( 3, "build: saving host as ip4: %s", hlist[i].Ipv4[0] )
				n.hosts[hlist[i].Ipv4[0]] = h
			}
			if len( hlist[i].Ipv6 ) > 0 && hlist[i].Ipv6[0] != "" {
				n.hosts[hlist[i].Ipv4[0]] = h
			}
		}
	}

	return
}

// -------------------- path finding ------------------------------------------------------------------------------------------------------

/*
	Find a set of connected switches that can be used as a path beteeen 
	hosts 1 and 2 (given by name; mac or ip).  Further, all links between from and the final switch must be able to 
	support the additional capacity indicated by inc_cap during the time window between
	commence and conclude (unix timestamps).

	If the network is 'split' a host may appear to be attached to multiple switches; one with a real connection and 
	the others are edge switches were we see an 'entry' point for the host from the portion of the network that we
	cannot visualise.  We must attempt to find a path between h1 using all of it's attached switches, and thus the 
	return is an array of paths rather than a single path.


	h1nm and h2nm are likely going to be ip addresses as the main function translates any names that would have
	come in from the requestor.  

	DEPRECATED: if the second host is "0.0.0.0", then we will return a path list containing every link we know about :)
*/
func (n *Network) find_path( h1nm *string, h2nm *string, commence int64, conclude int64, inc_cap int64 ) ( pcount int, path_list []*gizmos.Path ) {
	var (
		path	*gizmos.Path
		ssw 	*gizmos.Switch		// starting switch
		tsw 	*gizmos.Switch; 		// target's linked switch
		h1		*gizmos.Host
		h2		*gizmos.Host
		lnk		*gizmos.Link
		plidx	int = 0
		swidx	int = 0		// index into host's switch list
	)

	h1 = n.hosts[*h1nm]
	if h1 == nil {
		path_list = nil
		net_sheep.Baa( 1,  "find-path: cannot find host(1) in network -- not reported by SDNC? %s\n", *h1nm )
		return
	}
	h1nm = h1.Get_mac()			// must have the host's mac as our flowmods are at that level

	h2 = n.hosts[*h2nm]					// do the same for the second host
	if h2 == nil {
		path_list = nil
		net_sheep.Baa( 1,  "find-path: cannot find host(2) in network -- not reported by the SDNC? %s\n", *h2nm )
		return
	}
	h2nm = h2.Get_mac()

	if h1nm == nil || h2nm == nil {			// this has never happened, but be parinoid
		pcount = 0
		path_list = nil
		return
	}

	path_list = make( []*gizmos.Path, len( n.links ) )		// we cannot have more in our path than the number of links (needs to be changed as this isn't good in the long run)
	pcount = 0

	for {									// we'll break after we've looked at all of the connection points for h1 
		if plidx >= len( path_list ) {
			net_sheep.Baa( 0,  "internal error -- unable to find a path between hosts, loops in the graph?" )
			return
		}

		ssw, _ = h1.Get_switch_port( swidx )				// get next switch that lists h1 as attached; we'll work 'out' from it toward h2
		swidx++
		if ssw == nil {
			pcount = plidx
			return
		}

		if ssw.Has_host( h1nm )  &&  ssw.Has_host( h2nm ) {			// if both hosts are on the same switch, there's no path if they both have the same port
			p1 := h1.Get_port( ssw )
			p2 := h2.Get_port( ssw )
			if p1 != p2 {											// when ports differ we'll create/find the vlink between them
				lnk = n.find_vlink( *(ssw.Get_id()), p1, p2 )
				if lnk.Has_capacity( commence, conclude, inc_cap ) {		// room for the reservation
					net_sheep.Baa( 1, "path[%d]: found target on same switch, different ports: %s  %d, %d", plidx, ssw.To_str( ), h1.Get_port( ssw ), h2.Get_port( ssw ) )
					path = gizmos.Mk_path( h1, h2 )							// empty path
					path.Add_switch( ssw )
					path.Add_link( lnk )
	
					path_list[plidx] = path
					plidx++
				} else {
					net_sheep.Baa( 1, "path[%d]: hosts on same switch, virtual link cannot support bandwidth increase of %d", inc_cap )
				}
	
			}  else {					// debugging only
				net_sheep.Baa( 2,  "find-path: path[%d]: found target (%s) on same switch with same port: %s  %d, %d", plidx, *h2nm, ssw.To_str( ), p1, p2 )
				net_sheep.Baa( 2,  "find-path: host1-json= %s", h1.To_json( ) )
				net_sheep.Baa( 2,  "find-path: host2-json= %s", h2.To_json( ) )
			}
			
		} else {						// usual case, two named hosts and hosts are on different switches
			net_sheep.Baa( 2, "path[%d]: searching for path from switch: %s", plidx, ssw.To_str( ) )

			for sname := range n.switches {					// initialise the network for the walk
				n.switches[sname].Cost = 2147483647			// this should be large enough and allows cost to be int32
				n.switches[sname].Prev = nil
				n.switches[sname].Flags &= ^tegu.SWFL_VISITED
			}

			ssw.Cost = 0												// seed the cost in the source switch

			tsw = ssw.Path_to( h2nm, commence, conclude, inc_cap )		// discover the shortest path to terminating switch that has enough bandwidth
			if tsw != nil {
				path = gizmos.Mk_path( h1, h2 )
				path.Set_reverse( true )								// indicate that the path is saved in reverse order 
				net_sheep.Baa( 2,  "path[%d]: found target on %s (links follow)", plidx, tsw.To_str( ) )
				
				for ; tsw != nil ; {
					if tsw.Prev != nil {								// last node won't have a prev pointer so no link
						lnk = tsw.Prev.Get_link( tsw.Plink )
						path.Add_link( lnk )
					}	
					path.Add_switch( tsw )

					net_sheep.Baa( 2, "\t%s using link %d", tsw.Prev.To_str(), tsw.Plink )
					tsw = tsw.Prev
				}

				path_list[plidx] = path
				plidx++

			} /* else {				// debug only
				net_sheep.Baa( 1,  "path[%d]: did not find a path from %s -> %s using starting switch %s", plidx, *h1nm, *h2nm, ssw.To_str( ))
			}
			*/
		}
	}

	pcount = plidx			// shouldn't get here, but safety first
	return
}


// --------------------  info exchange/debugging  -----------------------------------------------------------------------------------------

/*
	Generate a json list of hosts which includes ip, name, switch(es) and port(s).
*/
func (n *Network) host_list( ) ( jstr string ) {
	var( 	
		sep 	string = ""
		hname	string = ""
		seen	map[string]bool
	)

	seen = make( map[string]bool )
	jstr = ` [ `						// an array of objects
	for _, h := range n.hosts {
		ip4, ip6 := h.Get_addresses()
		mac :=  h.Get_mac()						// track on this as we will always see this

		if seen[*mac] == false {
			seen[*mac] = true;					// we track hosts by both mac and ip so only show once

			if n.ip2vm[*ip4] != nil {
				hname = *n.ip2vm[*ip4]
			} else {
				hname = "unknown"
			}
			jstr += fmt.Sprintf( `%s { "name": %q, "mac": %q, "ip4": %q, "ip6": %q `, sep, hname, *(h.Get_mac()), *ip4, *ip6 )
			if nconns := h.Get_nconns(); nconns > 0 {
				jstr += `, "conns": [`
				sep = ""
				for i := 0; i < nconns; i++ {
					sw, port := h.Get_switch_port( i )
					if sw == nil {
						break
					}

					jstr += fmt.Sprintf( `%s { "switch": %q, "port": %d }`, sep, *(sw.Get_id( )), port )
					sep = ","
				}

				jstr += ` ]`
			}

			jstr += ` }`						// end of this host

			sep = ","
		}
	}

	jstr += ` ]`			// end of hosts array

	return
}


/*
	Generate a json representation of the network graph.
*/
func (n *Network) to_json( ) ( jstr string ) {
	var	sep string = ""

	jstr = `{ "netele": [ `

	for k := range n.switches {
		jstr += fmt.Sprintf( "%s%s", sep, n.switches[k].To_json( ) )
		sep = ","
	}

	jstr += "] }"

	return
}

// --------- public -------------------------------------------------------------------------------------------

/*
	to be executed as a go routine. 
	nch is the channel we are expected to listen on for api requests etc.
	sdn_host is the host name and port number where the sdn controller is running.
	(for now we assume the sdn-host is a floodlight host and invoke FL_ to build our graph)
*/
func Network_mgr( nch chan *ipc.Chmsg, sdn_host *string ) {
	var (
		act_net *Network
		req		*ipc.Chmsg
		max_link_cap	int64 = 0
		refresh	int = 30

		pcount	int
		path_list	[]*gizmos.Path
		ip2		*string
	)

	if *sdn_host  == "" {
		if sdn_host = cfg_data["default"]["sdn_host"]; sdn_host == nil {
			sdn_host = &default_sdn;
		}
	}

	net_sheep = bleater.Mk_bleater( 0, os.Stderr )		// allocate our bleater and attach it to the master
	net_sheep.Set_prefix( "netmgr" )
	tegu_sheep.Add_child( net_sheep )					// we become a child so that if the master vol is adjusted we'll react too

														// suss out config settings from our section
	if p := cfg_data["network"]["refresh"]; p != nil {
		refresh = clike.Atoi( *p ); 			
	}
	if p := cfg_data["network"]["link_max_cap"]; p != nil {
		max_link_cap = clike.Atoi64( *p )
	}
	if p := cfg_data["network"]["verbose"]; p != nil {
		net_sheep.Set_level(  uint( clike.Atoi( *p ) ) )
	}

														// enforce some sanity on config file settings
	if refresh <= 15 {
		net_sheep.Baa( 0, "WRN: refresh rate in config file (%d) was too small; set to 15s", refresh )
		refresh = 15
	}
	if max_link_cap <= 0 {
		max_link_cap = 1024 * 1024 * 1024 * 10							// if not in config file use 10Gbps
	}

	net_sheep.Baa( 1,  "network_mgr thread started: sdn_hpst=%s max_link_cap=%d refresh=%d", *sdn_host, max_link_cap, refresh )

	act_net = build( nil, sdn_host, max_link_cap )					// initial build of network graph; blocks and we don't enter loop until done (main depends on that)
	net_sheep.Baa( 1, "initial network graph has been built" )

	if refresh <= 10 {
		net_sheep.Baa( 0,  "default network refresh (30s) used because config value missing or invalid" )
		refresh = 30
	}
	tklr.Add_spot( int64( refresh ), nch, REQ_NETUPDATE, nil, ipc.FOREVER )		// add tickle spot to drive rebuild of network 
	
	for {
		select {					// assume we might have multiple channels in future
			case req = <- nch:
				req.State = nil				// nil state is OK, no error

				net_sheep.Baa( 3, "processing request %d", req.Msg_type )			// we seem to wedge in network, this will be chatty, but may help
				switch req.Msg_type {
					case REQ_NOOP:			// just ignore -- acts like a ping if there is a return channel

					case REQ_HASCAP:						// verify that there is capacity, and return the path, but don't allocate the path
						p := req.Req_data.( *gizmos.Pledge )
						h1, h2, commence, expiry, bandw_in, bandw_out := p.Get_values( )
						net_sheep.Baa( 1,  "has-capacity request received on channel  %s -> %s", h1, h2 )
						pcount, path_list = act_net.find_path( h1, h2, commence, expiry, bandw_in + bandw_out ); 

						if pcount > 0 {
							req.Response_data = path_list[:pcount]
							req.State = nil
						} else {
							req.Response_data = nil
							req.State = fmt.Errorf( "unable to generate a path; no capacity or no path" )
						}

					case REQ_RESERVE:
						p := req.Req_data.( *gizmos.Pledge )
						h1, h2, commence, expiry, bandw_in, bandw_out := p.Get_values( )
						net_sheep.Baa( 1,  "network: reservation request received: %s -> %s  from %d to %d", *h1, *h2, commence, expiry )

						ip1, err := act_net.name2ip( h1 )
						ip2 = nil
						if err == nil {
							ip2, err = act_net.name2ip( h2 )
						}

						if err == nil {
							net_sheep.Baa( 2,  "network: attempt to find path between  %s -> %s", *ip1, *ip2 )
							pcount, path_list = act_net.find_path( ip1, ip2, commence, expiry, bandw_in + bandw_out ); 

							if pcount > 0 {
								net_sheep.Baa( 1,  "network: acceptable path found:" )

								qid := p.Get_id()
								p.Set_qid( qid )					// add the queue id to the pledge

								for i := 0; i < pcount; i++ {		// set the queues for each path in the list (multiple paths if network is disjoint)
									net_sheep.Baa( 2,  "\tpath_list[%d]: %s -> %s\n\t%s", i, *h1, *h2, path_list[i].To_str( ) )
									path_list[i].Set_queue( qid, commence, expiry, bandw_in, bandw_out )
								}

								req.Response_data = path_list[:pcount]
								req.State = nil
							} else {
								req.Response_data = nil
								req.State = fmt.Errorf( "unable to generate a path; no capacity or no path" )
								net_sheep.Baa( 0,  "WRN: network: no path count %s", req.State )
							}
						} else {
							net_sheep.Baa( 0,  "WRN: network: unable to map to an IP address: %s",  err )
							req.State = fmt.Errorf( "unable to map host name to a known IP address: %s", err )
						}



					case REQ_DEL:							// delete the utilisation for the given reservation
						net_sheep.Baa( 1,  "network: deleting reservation" )
						p := req.Req_data.( *gizmos.Pledge )
						_, _, commence, expiry, bandw_in, bandw_out := p.Get_values( )
						pl := p.Get_path_list( )
						pcount := len( pl )

						for i := 0; i < pcount; i++ {
							net_sheep.Baa( 1,  "network: deleting path %d", i )
							path_list[i].Inc_utilisation( commence, expiry, -(bandw_in + bandw_out) )
						}

					case REQ_VM2IP:								// a new vm name/vm ID to ip address map 
						if req.Req_data != nil {
							act_net.vm2ip = req.Req_data.( map[string]*string )
							act_net.ip2vm = act_net.build_ip2vm( )
							net_sheep.Baa( 2, "vm2ip and ip2vm maps were updated, has %d entries", len( act_net.vm2ip ) )
						} else {
							net_sheep.Baa( 0, "vm2ip map was nil; not changed" )
						}

					case REQ_GEN_QMAP:							// generate a new queue setting map
						ts := req.Req_data.( int64 )			// time stamp for generation
						req.Response_data, req.State = act_net.gen_queue_map( ts )
						
					case REQ_GETIP:								// given a VM name or ID return the IP if we know it. 
						s := req.Req_data.( string )
						req.Response_data, req.State = act_net.name2ip( &s )		// returns ip or nil

					case REQ_GETLMAX:							// request for the max link allocation
						//req.Response_data = act_net.get_max_link_alloc( req.Req_data.( int64 ) )
						req.Response_data = nil;
						req.State = nil;

					case REQ_NETUPDATE:								// build a new network graph
						net_sheep.Baa( 2, "rebuilding network graph" )
						new_net := build( act_net, sdn_host, max_link_cap )
						if new_net != nil {
							vm2ip_map := act_net.vm2ip					// these don't come with the new graph; save old and add back 
							ip2vm_map := act_net.ip2vm
							act_net = new_net
							act_net.vm2ip = vm2ip_map
							act_net.ip2vm = ip2vm_map
							net_sheep.Baa( 1, "network graph rebuild completed" )		// timing during debugging
						} else {
							net_sheep.Baa( 1, "unable to update network graph -- SDNC down?" )
						}

						
					//	------------------ user api things ---------------------------------------------------------
					case REQ_NETGRAPH:							// dump the current network graph
						req.Response_data = act_net.to_json()

					case REQ_HOSTLIST:							// json list of hosts with name, ip, switch id and port
						req.Response_data = act_net.host_list( )

					case REQ_LISTCONNS:							// for a given host spit out the switch(es) and port(s) 
						hname := req.Req_data.( *string )
						host := act_net.hosts[*hname]
						if host != nil {
							req.Response_data = host.Ports2json( ); 
						} else {
							req.Response_data = nil			// assume failure
							req.State = fmt.Errorf( "did not find host: %s", *hname )

							net_sheep.Baa( 2, "looking up name for listconns: %s", *hname )
							hname = act_net.vm2ip[*hname]		// maybe they sent a vm ID or name
							if hname == nil || *hname == "" {
								net_sheep.Baa( 2, "unable to find name in vm2ip table" )

								if net_sheep.Get_level() > 2 {
									for k, v := range act_net.vm2ip {
										net_sheep.Baa( 3, "vm2ip[%s] = %s", k, *v );
									}
								}
							} else {
								net_sheep.Baa( 2, "name found in vm2ip table translated to: %s, looking up  host", *hname )
								host = act_net.hosts[*hname]
								if host != nil {
									req.Response_data = host.Ports2json( ); 
									req.State = nil
								} else {
									net_sheep.Baa( 2, "unable to find host entry for: %s", *hname )
								}
							}
						}

					default:
						net_sheep.Baa( 1,  "unknown request received on channel: %d", req.Msg_type )
				}

				net_sheep.Baa( 3, "processing request complete %d", req.Msg_type )
				if req.Response_ch != nil {				// if response needed; send the request (updated) back 
					req.Response_ch <- req
				}

		}
	}
}

