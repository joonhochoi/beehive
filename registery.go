package actor

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/coreos/go-etcd/etcd"
	"github.com/golang/glog"
)

type registery struct {
	*etcd.Client
	prefix string
	ttl    uint64
}

func (g registery) connected() bool {
	return g.Client == nil
}

type regVal struct {
	StageId StageId `json:"stage_id"`
	RcvrId  uint32  `json:"rcvr_id"`
}

func (this *regVal) Eq(that *regVal) bool {
	return this.StageId == that.StageId && this.RcvrId == that.RcvrId
}

func unmarshallRegVal(d string) (regVal, error) {
	var v regVal
	err := json.Unmarshal([]byte(d), &v)
	return v, err
}

func unmarshallRegValOrFail(d string) regVal {
	v, err := unmarshallRegVal(d)
	if err != nil {
		glog.Fatalf("Cannot unmarshall registery value %v: %v", d, err)
	}
	return v
}

func marshallRegVal(v regVal) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func marshallRegValOrFail(v regVal) string {
	d, err := marshallRegVal(v)
	if err != nil {
		glog.Fatalf("Cannot marshall registery value %v: %v", v, err)
	}
	return d
}

const (
	keyFmtStr    = "/theatre/%s/%s/%s"
	expireAction = "expire"
	lockFileName = "__lock__"
)

func (g registery) path(elem ...string) string {
	return g.prefix + "/" + strings.Join(elem, "/")
}

func (g registery) lockActor(id ReceiverId) error {
	// TODO(soheil): For lock and unlock we can use etcd indices but
	// v.Temp might be changed by the app. Check this and fix it if possible.
	v := regVal{
		StageId: id.StageId,
		RcvrId:  id.RcvrId,
	}
	k := g.path(string(id.ActorName), lockFileName)

	for {
		_, err := g.Create(k, marshallRegValOrFail(v), g.ttl)
		if err == nil {
			return nil
		}

		_, err = g.Watch(k, 0, false, nil, nil)
		if err != nil {
			return err
		}
	}
}

func (g registery) unlockActor(id ReceiverId) error {
	v := regVal{
		StageId: id.StageId,
		RcvrId:  id.RcvrId,
	}
	k := g.path(string(id.ActorName), lockFileName)

	res, err := g.Get(k, false, false)
	if err != nil {
		return err
	}

	tempV := unmarshallRegValOrFail(res.Node.Value)
	if !v.Eq(&tempV) {
		return errors.New(
			fmt.Sprintf("Unlocking someone else's lock: %v, %v", v, tempV))
	}

	_, err = g.Delete(k, false)
	if err != nil {
		return err
	}

	return nil
}

func (g registery) storeOrGet(id ReceiverId, ms MapSet) regVal {
	g.lockActor(id)
	defer g.unlockActor(id)

	sort.Sort(ms)

	v := regVal{
		StageId: id.StageId,
		RcvrId:  id.RcvrId,
	}
	mv := marshallRegValOrFail(v)
	validate := false
	for _, dk := range ms {
		k := fmt.Sprintf(keyFmtStr, id.ActorName, dk.Dict, dk.Key)
		fmt.Println(k)
		res, err := g.Get(k, false, false)
		if err != nil {
			continue
		}

		resV := unmarshallRegValOrFail(res.Node.Value)
		if resV.Eq(&v) {
			continue
		}

		if validate {
			glog.Fatalf("Incosistencies for receiver %v: %v, %v", id, v, resV)
		}

		v = resV
		mv = res.Node.Value
		validate = true
	}

	for _, dk := range ms {
		k := fmt.Sprintf(keyFmtStr, id.ActorName, dk.Dict, dk.Key)
		g.Create(k, mv, g.ttl)
	}

	return v
}