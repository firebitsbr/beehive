package bh

import (
	"errors"
	"fmt"

	"github.com/golang/glog"
)

type QeeID struct {
	Hive HiveID
	App  AppName
}

// An applictaion's queen bee is the light weight thread that routes messags
// through the bees of that application.
type qee struct {
	dataCh    chan msgAndHandler
	ctrlCh    chan LocalCmd
	ctx       mapContext
	lastBeeID uint64
	idToBees  map[BeeID]bee
	keyToBees map[CellKey]bee
}

func (q *qee) state() State {
	if q.ctx.state == nil {
		q.ctx.state = newState(q.ctx.app)
	}
	return q.ctx.state
}

func (q *qee) setDetached(d *detachedBee) error {
	q.idToBees[d.id()] = d
	return nil
}

func (q *qee) detachedBees() []*detachedBee {
	ds := make([]*detachedBee, 0, len(q.idToBees))
	for id, b := range q.idToBees {
		if !id.Detached {
			continue
		}
		ds = append(ds, b.(*detachedBee))
	}
	return ds
}

func (q *qee) start() {
	for {
		select {
		case d, ok := <-q.dataCh:
			if !ok {
				return
			}
			q.handleMsg(d)

		case cmd, ok := <-q.ctrlCh:
			if !ok {
				return
			}
			q.handleCmd(cmd)
		}
	}
}

func (q *qee) closeChannels() {
	close(q.dataCh)
	close(q.ctrlCh)
}

func (q *qee) stopBees() {
	stopCh := make(chan CmdResult)
	stopCmd := LocalCmd{
		CmdType: stopCmd,
		CmdData: nil,
		ResCh:   stopCh,
	}

	for _, b := range q.idToBees {
		b.enqueCmd(stopCmd)

		_, err := (<-stopCh).get()
		if err != nil {
			glog.Errorf("Error in stopping a bee: %v", err)
		}
	}
}

func (q *qee) handleCmd(cmd LocalCmd) {
	switch cmd.CmdType {
	case stopCmd:
		glog.V(3).Infof("Stopping bees of %p", q)
		q.stopBees()
		q.closeChannels()
		cmd.ResCh <- CmdResult{}

	case findBeeCmd:
		id := cmd.CmdData.(BeeID)
		r, ok := q.idToBees[id]
		if ok {
			cmd.ResCh <- CmdResult{r, nil}
			return
		}

		err := fmt.Errorf("No bee found: %+v", id)
		cmd.ResCh <- CmdResult{nil, err}

	case createBeeCmd:
		r := q.newLocalBee()
		glog.V(2).Infof("Created a new local bee: %+v", r.id())
		cmd.ResCh <- CmdResult{r.id(), nil}

	case migrateBeeCmd:
		m := cmd.CmdData.(migrateBeeCmdData)
		q.migrate(m.From, m.To, cmd.ResCh)

	case replaceBeeCmd:
		d := cmd.CmdData.(replaceBeeCmdData)
		q.replaceBee(d, cmd.ResCh)

	case lockMappedCellsCmd:
		d := cmd.CmdData.(lockMappedCellsData)
		id := q.lock(d.MappedCells, d.BeeID, true)
		if id != d.BeeID {
			cmd.ResCh <- CmdResult{nil, errors.New("Cannot lock mapset")}
			return
		}

		q.lockLocally(q.findOrCreateBee(id), d.MappedCells...)
		cmd.ResCh <- CmdResult{id, nil}

	case startDetachedCmd:
		h := cmd.CmdData.(DetachedHandler)
		b := q.newDetachedBee(h)
		q.idToBees[b.id()] = b
		go b.start()

		if cmd.ResCh != nil {
			cmd.ResCh <- CmdResult{Data: b.id()}
		}
	}
}

func (q *qee) beeByKey(dk CellKey) (bee, bool) {
	r, ok := q.keyToBees[dk]
	return r, ok
}

func (q *qee) beeByID(id BeeID) (bee, bool) {
	r, ok := q.idToBees[id]
	return r, ok
}

func (q *qee) lockLocally(bee bee, dks ...CellKey) {
	for _, dk := range dks {
		q.keyToBees[dk] = bee
	}
}

func (q *qee) syncBees(ms MappedCells, bee bee) {
	for _, dictKey := range ms {
		dkRcvr, ok := q.beeByKey(dictKey)
		if !ok {
			q.lockKey(dictKey, bee)
			continue
		}

		if dkRcvr == bee {
			continue
		}

		glog.Fatalf("Incosistent shards for keys %v in MappedCells %v", dictKey,
			ms)
	}
}

func (q *qee) anyBee(ms MappedCells) bee {
	for _, dictKey := range ms {
		bee, ok := q.beeByKey(dictKey)
		if ok {
			return bee
		}
	}

	return nil
}

func (q *qee) callMap(mh msgAndHandler) (ms MappedCells) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("Error in map of %s: %v", q.ctx.app.Name(), r)
			ms = nil
		}
	}()

	return mh.handler.Map(mh.msg, &q.ctx)
}

func (q *qee) handleMsg(mh msgAndHandler) {
	if mh.msg.isUnicast() {
		glog.V(2).Infof("Unicast msg: %+v", mh.msg)
		bee, ok := q.beeByID(mh.msg.To())
		if !ok {
			if q.isLocalBee(mh.msg.To()) {
				glog.Fatalf("Cannot find a local bee: %+v", mh.msg.To())
			}

			bee = q.findOrCreateBee(mh.msg.To())
		}

		if mh.handler == nil && !mh.msg.To().Detached {
			glog.Fatalf("Handler cannot be nil for bees: %+v, %+v", mh, mh.msg)
		}

		bee.enqueMsg(mh)
		return
	}

	glog.V(2).Infof("Broadcast msg: %+v", mh.msg)

	mapSet := q.callMap(mh)
	if mapSet == nil {
		glog.V(2).Infof("Message dropped: %+v", mh)
		return
	}

	if mapSet.LocalBroadcast() {
		glog.V(2).Infof("Sending a message to all local bees: %v", mh.msg)
		for _, bee := range q.idToBees {
			bee.enqueMsg(mh)
		}
		return
	}

	bee := q.anyBee(mapSet)
	if bee == nil {
		bee = q.newBeeForMappedCells(mapSet)
	} else {
		q.syncBees(mapSet, bee)
	}

	glog.V(2).Infof("Sending to bee: %v", bee.id())
	bee.enqueMsg(mh)
}

func (q *qee) nextBeeID(detached bool) BeeID {
	q.lastBeeID++
	return BeeID{
		AppName:  q.ctx.app.Name(),
		HiveID:   q.ctx.hive.ID(),
		ID:       q.lastBeeID,
		Detached: detached,
	}
}

// lock locks the map set for the given bee. If the mapset is already locked,
// it will return the ID of the owner. If force forced it will force the local
// bee to lock the map set.
func (q *qee) lock(mapSet MappedCells, bee BeeID, force bool) BeeID {
	if q.ctx.hive.isolated() {
		return bee
	}

	var prevBee BeeID
	if force {
		// FIXME(soheil): We should migrate the previous owner first.
		prevBee = q.ctx.hive.registery.set(bee, mapSet)
	} else {
		prevBee = q.ctx.hive.registery.storeOrGet(bee, mapSet)
	}

	return prevBee
}

func (q *qee) lockKey(dk CellKey, bee bee) bool {
	q.lockLocally(bee, dk)
	if q.ctx.hive.isolated() {
		return true
	}

	q.ctx.hive.registery.storeOrGet(bee.id(), []CellKey{dk})

	return true
}

func (q *qee) isLocalBee(id BeeID) bool {
	return q.ctx.hive.ID() == id.HiveID
}

func (q *qee) defaultLocalBee(id BeeID) localBee {
	return localBee{
		dataCh: make(chan msgAndHandler, cap(q.dataCh)),
		ctrlCh: make(chan LocalCmd, cap(q.ctrlCh)),
		ctx:    q.ctx.newRcvContext(),
		bID:    id,
		qee:    q,
	}
}

func (q *qee) proxyFromLocal(id BeeID, lBee *localBee) (*proxyBee,
	error) {

	if q.isLocalBee(id) {
		return nil, fmt.Errorf("Bee ID is a local ID: %+v", id)
	}

	if r, ok := q.beeByID(id); ok {
		return nil, fmt.Errorf("Bee already exists: %+v", r)
	}

	r := &proxyBee{
		localBee: *lBee,
	}
	r.bID = id
	r.ctx.bee = r
	q.idToBees[id] = r
	q.idToBees[lBee.id()] = r
	return r, nil
}

func (q *qee) localFromProxy(id BeeID, pBee *proxyBee) (*localBee,
	error) {

	if !q.isLocalBee(id) {
		return nil, fmt.Errorf("Bee ID is a proxy ID: %+v", id)
	}

	if r, ok := q.beeByID(id); ok {
		return nil, fmt.Errorf("Bee already exists: %+v", r)
	}

	r := pBee.localBee
	r.bID = id
	r.ctx.bee = &r
	q.idToBees[id] = &r
	q.idToBees[pBee.id()] = &r
	return &r, nil
}

func (q *qee) newLocalBee() bee {
	return q.findOrCreateBee(q.nextBeeID(false))
}

func (q *qee) findOrCreateBee(id BeeID) bee {
	if r, ok := q.beeByID(id); ok {
		return r
	}

	l := q.defaultLocalBee(id)

	var bee bee
	if q.isLocalBee(id) {
		b := &l
		b.ctx.bee = b
		bee = b
	} else {
		b := &proxyBee{
			localBee: l,
		}
		b.ctx.bee = b
		bee = b

		startHeartbeatBee(id, q.ctx.hive)
	}

	q.idToBees[id] = bee
	go bee.start()

	return bee
}

func (q *qee) newDetachedBee(h DetachedHandler) *detachedBee {
	id := q.nextBeeID(true)
	d := &detachedBee{
		localBee: q.defaultLocalBee(id),
		h:        h,
	}
	d.ctx.bee = d
	return d
}

func (q *qee) newBeeForMappedCells(mapSet MappedCells) bee {
	newBeeID := q.nextBeeID(false)
	beeID := q.lock(mapSet, newBeeID, false)
	if newBeeID != beeID {
		q.lastBeeID--
	}

	bee := q.findOrCreateBee(beeID)
	q.lockLocally(bee, mapSet...)
	return bee
}

func (q *qee) mapSetOfBee(id BeeID) MappedCells {
	ms := MappedCells{}
	for k, r := range q.keyToBees {
		if r.id() == id {
			ms = append(ms, k)
		}
	}
	return ms
}

func (q *qee) migrate(beeID BeeID, to HiveID, resCh chan CmdResult) {
	if beeID.Detached {
		err := fmt.Errorf("Cannot migrate a detached: %+v", beeID)
		resCh <- CmdResult{nil, err}
		return
	}

	oldBee, ok := q.beeByID(beeID)
	if !ok {
		err := fmt.Errorf("Bee not found: %+v", oldBee)
		resCh <- CmdResult{nil, err}
		return
	}

	stopCh := make(chan CmdResult)
	oldBee.enqueCmd(LocalCmd{
		CmdType: stopCmd,
		ResCh:   stopCh,
	})
	_, err := (<-stopCh).get()
	if err != nil {
		resCh <- CmdResult{nil, err}
		return
	}

	glog.V(2).Infof("Received stopped: %+v", oldBee)

	// TODO(soheil): There is a possibility of a deadlock. If the number of
	// migrrations pass the control channel's buffer size.

	prx := NewProxy(to)

	id := BeeID{HiveID: to, AppName: beeID.AppName}

	cmd := RemoteCmd{
		CmdType: createBeeCmd,
		CmdTo:   id,
	}

	data, err := prx.SendCmd(&cmd)
	if err != nil {
		glog.Errorf("Error in creating a new bee: %s", err)
		resCh <- CmdResult{nil, err}
		return
	}

	id = data.(BeeID)

	glog.V(2).Infof("Created a new bee for migration: %+v", id)

	newBee, err := q.proxyFromLocal(id, oldBee.(*localBee))
	if err != nil {
		resCh <- CmdResult{nil, err}
		return
	}

	glog.V(2).Infof("Created a proxy for the new bee: %+v", newBee)

	mapSet := q.mapSetOfBee(oldBee.id())
	cmd = RemoteCmd{
		CmdType: replaceBeeCmd,
		CmdTo:   id,
		CmdData: replaceBeeCmdData{
			OldBee:      oldBee.id(),
			NewBee:      newBee.id(),
			State:       oldBee.state().(*inMemoryState),
			MappedCells: mapSet,
		},
	}

	_, err = prx.SendCmd(&cmd)
	if err != nil {
		glog.Errorf("Error in replacing the bee: %s", err)
		return
	}

	q.lockLocally(newBee, mapSet...)

	go newBee.start()
	resCh <- CmdResult{newBee, nil}
}

func (q *qee) replaceBee(d replaceBeeCmdData, resCh chan CmdResult) {
	if !q.isLocalBee(d.NewBee) {
		err := fmt.Errorf("Cannot replace with a non-local bee: %+v", d.NewBee)
		resCh <- CmdResult{nil, err}
		return
	}

	b, ok := q.beeByID(d.NewBee)
	if !ok {
		err := fmt.Errorf("Cannot find bee: %+v", d.NewBee)
		resCh <- CmdResult{nil, err}
		return
	}

	newState := b.state()
	for name, oldDict := range d.State.Dicts {
		newDict := newState.Dict(DictName(name))
		oldDict.ForEach(func(k Key, v Value) {
			newDict.Put(k, v)
		})
	}
	glog.V(2).Infof("Replicated the state of %+v on %+v", d.OldBee, d.NewBee)

	q.ctx.hive.registery.set(d.NewBee, d.MappedCells)
	glog.V(2).Infof("Locked the mapset %+v for %+v", d.MappedCells, d.NewBee)

	q.lockLocally(b, d.MappedCells...)

	resCh <- CmdResult{b.id(), nil}
}