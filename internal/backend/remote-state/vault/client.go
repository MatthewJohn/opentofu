// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package consul

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/opentofu/opentofu/internal/states/remote"
	"github.com/opentofu/opentofu/internal/states/statemgr"
)

const (
	lockSuffix     = "/.lock"
	lockInfoSuffix = "/.lockinfo"

	// The Session TTL associated with this lock.
	lockSessionTTL = "15s"

	dataKey = "data"

	// the delay time from when a session is lost to when the
	// lock is released by the server
	lockDelay = 5 * time.Second
	// interval between attempts to reacquire a lost lock
	lockReacquireInterval = 2 * time.Second
)

var lostLockErr = errors.New("consul lock was lost")

// RemoteClient is a remote client that stores data in Consul.
type RemoteClient struct {
	Mount string
	Name  string
	GZip  bool

	mu     sync.Mutex
	Client *vaultapi.Client
	// lockState is true if we're using locks
	lockState bool

	// The index of the last state we wrote.
	// If this is > 0, Put will perform a CAS to ensure that the state wasn't
	// changed during the operation. This is important even with locks, because
	// if the client loses the lock for some reason, then reacquires it, we
	// need to make sure that the state was not modified.
	modifyIndex uint64

	// consulLock *vaultapi.Lock
	lockCh <-chan struct{}

	info *statemgr.LockInfo

	// cancel our goroutine which is monitoring the lock to automatically
	// reacquire it when possible.
	monitorCancel context.CancelFunc
	monitorWG     sync.WaitGroup

	// sessionCancel cancels the Context use for session.RenewPeriodic, and is
	// called when unlocking, or before creating a new lock if the lock is
	// lost.
	sessionCancel context.CancelFunc
}

func (c *RemoteClient) Get() (*remote.Payload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	kv := c.Client.KVv2(c.Mount)

	chunked, hash, chunks, secret, err := c.chunkedMode()

	// If vault error contains no secret, return empty state
	if strings.Contains(err.Error(), "secret not found") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	c.modifyIndex = uint64(secret.VersionMetadata.Version)

	var payload string
	if chunked {
		for _, chunkName := range chunks {
			ctx := context.TODO()
			secret, err := kv.Get(ctx, chunkName)
			if err != nil {
				return nil, err
			}
			if secret == nil {
				return nil, fmt.Errorf("Key %q could not be found", chunkName)
			}
			if val, ok := secret.Data["data"]; ok {
				if f, ok := val.(string); ok {
					payload = payload + f
				} else {
					return nil, fmt.Errorf("Invalid chunk type, not string: %v", chunkName)
				}
			} else {
				return nil, fmt.Errorf("Could not find 'data' key: %v", chunkName)
			}
		}
	} else {
		if val, ok := secret.Data["data"]; ok {
			if f, ok := val.(string); ok {
				payload = f
			} else {
				return nil, fmt.Errorf("Invalid chunk type, not string: %v", c.Name)
			}
		} else {
			return nil, fmt.Errorf("Could not find 'data' key: %v", c.Name)
		}
	}

	// If the payload starts with 0x1f, it's gzip, not json
	if len(payload) >= 1 && payload[0] == '\x1f' {
		payload, err = uncompressState(payload)
		if err != nil {
			return nil, err
		}
	}

	md5 := md5.Sum([]byte(payload))

	if hash != "" && fmt.Sprintf("%x", md5) != hash {
		return nil, fmt.Errorf("The remote state does not match the expected hash")
	}

	return &remote.Payload{
		Data: []byte(payload),
		MD5:  md5[:],
	}, nil
}

func (c *RemoteClient) Put(data []byte) error {
	// The state can be stored in 4 different ways, based on the payload size
	// and whether the user enabled gzip:
	//  - single entry mode with plain JSON: a single JSON is stored at
	//	  "tfstate/my_project"
	//  - single entry mode gzip: the JSON payload is first gziped and stored at
	//    "tfstate/my_project"
	//  - chunked mode with plain JSON: the JSON payload is split in pieces and
	//    stored like so:
	//       - "tfstate/my_project" -> a JSON payload that contains the path of
	//         the chunks and an MD5 sum like so:
	//              {
	//              	"current-hash": "abcdef1234",
	//              	"chunks": [
	//              		"tfstate/my_project/tfstate.abcdef1234/0",
	//              		"tfstate/my_project/tfstate.abcdef1234/1",
	//              		"tfstate/my_project/tfstate.abcdef1234/2",
	//              	]
	//              }
	//       - "tfstate/my_project/tfstate.abcdef1234/0" -> The first chunk
	//       - "tfstate/my_project/tfstate.abcdef1234/1" -> The next one
	//       - ...
	//  - chunked mode with gzip: the same system but we gziped the JSON payload
	//    before splitting it in chunks
	//
	// When overwritting the current state, we need to clean the old chunks if
	// we were in chunked mode (no matter whether we need to use chunks for the
	// new one). To do so based on the 4 possibilities above we look at the
	// value at "tfstate/my_project" and if it is:
	//  - absent then it's a new state and there will be nothing to cleanup,
	//  - not a JSON payload we were in single entry mode with gzip so there will
	// 	  be nothing to cleanup
	//  - a JSON payload, then we were either single entry mode with plain JSON
	//    or in chunked mode. To differentiate between the two we look whether a
	//    "current-hash" key is present in the payload. If we find one we were
	//    in chunked mode and we will need to remove the old chunks (whether or
	//    not we were using gzip does not matter in that case).

	c.mu.Lock()
	defer c.mu.Unlock()

	kv := c.Client.KVv2(c.Mount)
	ctx := context.TODO()

	// First we determine what mode we were using and to prepare the cleanup
	chunked, _, oldChunks, _, err := c.chunkedMode()
	if err != nil {
		return err
	}
	cleanupOldChunks := func() {}
	if chunked {
		cleanupOldChunks = func() {
			for _, chunkName := range oldChunks {
				ctx := context.TODO()
				kv.Delete(ctx, chunkName)
			}
		}
	}

	payload := string(data)
	if c.GZip {
		if compressedState, err := compressState(payload); err == nil {
			payload = compressedState
		} else {
			return err
		}
	}

	// The payload may be too large to store in a single KV entry in Consul. We
	// could try to determine whether it will fit or not before sending the
	// request but since we are using the Transaction API and not the KV API,
	// it grows by about a 1/3 when it is base64 encoded plus the overhead of
	// the fields specific to the Transaction API.
	// Rather than trying to calculate the overhead (which could change from
	// one version of Consul to another, and between Consul Community Edition
	// and Consul Enterprise), we try to send the whole state in one request, if
	// it fails because it is too big we then split it in chunks and send each
	// chunk separately.
	// When splitting in chunks, we make each chunk 524288 bits, which is the
	// default max size for raft. If the user changed it, we still may send
	// chunks too big and fail but this is not a setting that should be fiddled
	// with anyway.

	store := func(payload map[string]interface{}) error {
		var secret *vaultapi.KVSecret = nil
		var err error = nil
		if c.modifyIndex == 0 {
			secret, err = kv.Put(ctx, c.Name, payload)
		} else {
			secret, err = kv.Patch(ctx, c.Name, payload)
		}
		if err != nil {
			return err
		}
		c.modifyIndex = uint64(secret.VersionMetadata.Version)

		// We remove all the old chunks
		cleanupOldChunks()

		return nil
	}

	if err = store(map[string]interface{}{"data": payload}); err == nil {
		// The payload was small enough to be stored
		return nil
	} else if !strings.Contains(err.Error(), "too large") {
		// We failed for some other reason, report this to the user
		return err
	}

	// The payload was too large so we split it in multiple chunks

	md5 := md5.Sum(data)
	chunks := split(payload, 524288)
	chunkPaths := make([]string, 0)

	// First we write the new chunks
	for i, p := range chunks {
		path := strings.TrimRight(c.Mount, "/") + fmt.Sprintf("/tfstate.%x/%d", md5, i)
		chunkPaths = append(chunkPaths, path)
		_, err := kv.Put(ctx, path, map[string]interface{}{
			"data": p,
		})

		if err != nil {
			return err
		}
	}

	// Then we update the link to point to the new chunks
	return store(map[string]interface{}{
		"current-hash": fmt.Sprintf("%x", md5),
		"chunks":       chunkPaths,
	})
}

func (c *RemoteClient) Delete() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	kv := c.Client.KVv2(c.Mount)
	ctx := context.TODO()

	chunked, _, chunks, _, err := c.chunkedMode()
	if err != nil {
		return err
	}

	err = kv.Delete(ctx, c.Name)

	// If there were chunks we need to remove them
	if chunked {
		for _, chunk := range chunks {
			kv.Delete(ctx, chunk)
		}
	}

	return err
}

func (c *RemoteClient) lockPath() string {
	// we sanitize the path for the lock as Consul does not like having
	// two consecutive slashes for the lock path
	return c.Name + ".lock"
}

func (c *RemoteClient) putLockInfo(info *statemgr.LockInfo) error {
	info.Path = c.Name
	info.Created = time.Now().UTC()

	kv := c.Client.KVv2(c.Mount)
	ctx := context.TODO()

	val := info.Marshal()
	var vaultData map[string]interface{}
	err := json.Unmarshal(val, &vaultData)
	if err != nil {
		return err
	}

	_, err = kv.Put(ctx, c.lockPath()+lockInfoSuffix, vaultData)

	// _, err := kv.Put(ctx, c.lockPath()+lockInfoSuffix, map[string]interface{}{
	// 	"Created":   info.Created,
	// 	"ID":        info.ID,
	// 	"Info":      info.Info,
	// 	"Operation": info.Operation,
	// 	"Path":      info.Path,
	// 	"Version":   info.Version,
	// 	"Who":       info.Who,
	// })

	return err
}

func (c *RemoteClient) getLockInfo() (*statemgr.LockInfo, error) {
	path := c.lockPath() + lockInfoSuffix

	kv := c.Client.KVv2(c.Mount)
	ctx := context.TODO()
	secret, err := kv.Get(ctx, path)

	if err != nil {
		return nil, err
	}
	if secret == nil {
		return nil, nil
	}

	li := &statemgr.LockInfo{}
	byteData, err := json.Marshal(secret.Data)
	err = json.Unmarshal(byteData, li)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling lock info: %w", err)
	}

	return li, nil
}

func (c *RemoteClient) Lock(info *statemgr.LockInfo) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lockState {
		return "", nil
	}

	c.info = info

	// These checks only are to ensure we strictly follow the specification.
	// OpenTofu shouldn't ever re-lock, so provide errors for the 2 possible
	// states if this is called.
	select {
	case <-c.lockCh:
		// We had a lock, but lost it.
		return "", errors.New("lost vault lock, cannot re-lock")
	default:
		if c.lockCh != nil {
			// we have an active lock already
			return "", fmt.Errorf("state %q/%q already locked", c.Mount, c.Name)
		}
	}

	return c.lock()
}

// the lock implementation.
// Only to be called while holding Client.mu
func (c *RemoteClient) lock() (string, error) {
	return "abdef", nil
	// @TODO
	// // We create a new session here, so it can be canceled when the lock is
	// // lost or unlocked.
	// lockSession, err := c.createSession()
	// if err != nil {
	// 	return "", err
	// }

	// // Lock perform lock by performing the following, making use of CAS:
	// // 1. Obtain latest version of lock secret
	// // 2. Ensure latest version is either non-existent or deleted
	// // 3. Increment the CAS
	// // 4. Create new version with inremented CAS
	// // 5. If the write is allowed, then we have the lock
	// // 6. If Vault rejects the secret, then someone else has locked

	// // store the session ID for correlation with consul logs
	// c.info.Info = "consul session: " + lockSession

	// // A random lock ID has been generated but we override it with the session
	// // ID as this will make it easier to manually invalidate the session
	// // if needed.
	// c.info.ID = lockSession

	// opts := &vaultapi.LockOptions{
	// 	Key:     c.lockPath() + lockSuffix,
	// 	Session: lockSession,

	// 	// only wait briefly, so tofu has the choice to fail fast or
	// 	// retry as needed.
	// 	LockWaitTime: time.Second,
	// 	LockTryOnce:  true,

	// 	// Don't let the lock monitor give up right away, as it's possible the
	// 	// session is still OK. While the session is refreshed at a rate of
	// 	// TTL/2, the lock monitor is an idle blocking request and is more
	// 	// susceptible to being closed by a lower network layer.
	// 	MonitorRetries: 5,
	// 	//
	// 	// The delay between lock monitor retries.
	// 	// While the session has a 15s TTL plus a 5s wait period on a lost
	// 	// lock, if we can't get our lock back in 10+ seconds something is
	// 	// wrong so we're going to drop the session and start over.
	// 	MonitorRetryTime: 2 * time.Second,
	// }

	// c.consulLock, err = c.Client.LockOpts(opts)
	// if err != nil {
	// 	return "", err
	// }

	// lockErr := &statemgr.LockError{}

	// lockCh, err := c.consulLock.Lock(make(chan struct{}))
	// if err != nil {
	// 	lockErr.Err = err
	// 	return "", lockErr
	// }

	// if lockCh == nil {
	// 	lockInfo, e := c.getLockInfo()
	// 	if e != nil {
	// 		lockErr.Err = e
	// 		return "", lockErr
	// 	}

	// 	lockErr.Info = lockInfo

	// 	return "", lockErr
	// }

	// c.lockCh = lockCh

	// err = c.putLockInfo(c.info)
	// if err != nil {
	// 	if unlockErr := c.unlock(c.info.ID); unlockErr != nil {
	// 		err = multierror.Append(err, unlockErr)
	// 	}

	// 	return "", err
	// }

	// // Start a goroutine to monitor the lock state.
	// // If we lose the lock to due communication issues with the consul agent,
	// // attempt to immediately reacquire the lock. Put will verify the integrity
	// // of the state by using a CAS operation.
	// ctx, cancel := context.WithCancel(context.Background())
	// c.monitorCancel = cancel
	// c.monitorWG.Add(1)
	// go func() {
	// 	defer c.monitorWG.Done()
	// 	select {
	// 	case <-c.lockCh:
	// 		log.Println("[ERROR] lost consul lock")
	// 		for {
	// 			c.mu.Lock()
	// 			// We lost our lock, so we need to cancel the session too.
	// 			// The CancelFunc is only replaced while holding Client.mu, so
	// 			// this is safe to call here. This will be replaced by the
	// 			// lock() call below.
	// 			c.sessionCancel()

	// 			c.consulLock = nil
	// 			_, err := c.lock()
	// 			c.mu.Unlock()

	// 			if err != nil {
	// 				// We failed to get the lock, keep trying as long as
	// 				// tofu is running. There may be changes in progress,
	// 				// so there's no use in aborting. Either we eventually
	// 				// reacquire the lock, or a Put will fail on a CAS.
	// 				log.Printf("[ERROR] could not reacquire lock: %s", err)
	// 				time.Sleep(lockReacquireInterval)

	// 				select {
	// 				case <-ctx.Done():
	// 					return
	// 				default:
	// 				}
	// 				continue
	// 			}

	// 			// if the error was nil, the new lock started a new copy of
	// 			// this goroutine.
	// 			return
	// 		}

	// 	case <-ctx.Done():
	// 		return
	// 	}
	// }()

	// if testLockHook != nil {
	// 	testLockHook()
	// }

	// return c.info.ID, nil
}

// called after a lock is acquired
var testLockHook func()

func (c *RemoteClient) createSession() (string, error) {
	return "abcdefg", nil
	// @TODO
	// // create the context first. Even if the session creation fails, we assume
	// // that the CancelFunc is always callable.
	// ctx, cancel := context.WithCancel(context.Background())
	// c.sessionCancel = cancel

	// session := c.Client.Session()
	// se := &vaultapi.SessionEntry{
	// 	Name:      vaultapi.DefaultLockSessionName,
	// 	TTL:       lockSessionTTL,
	// 	LockDelay: lockDelay,
	// }

	// id, _, err := session.Create(se, nil)
	// if err != nil {
	// 	return "", err
	// }

	// log.Println("[INFO] created consul lock session", id)

	// // keep the session renewed
	// go session.RenewPeriodic(lockSessionTTL, id, nil, ctx.Done())

	// return id, nil
}

func (c *RemoteClient) Unlock(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lockState {
		return nil
	}

	return c.unlock(id)
}

// the unlock implementation.
// Only to be called while holding Client.mu
func (c *RemoteClient) unlock(id string) error {
	// This method can be called in two circumstances:
	// - when the plan apply or destroy operation finishes and the lock needs to be released,
	// the watchdog stopped and the session closed
	// - when the user calls `tofu force-unlock <lock_id>` in which case
	// we only need to release the lock.
	return nil

	// @TODO
	// if c.consulLock == nil || c.lockCh == nil {
	// 	// The user called `tofu force-unlock <lock_id>`, we just destroy
	// 	// the session which will release the lock, clean the KV store and quit.

	// 	_, err := c.Client.Session().Destroy(id, nil)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	// We ignore the errors that may happen during cleanup
	// 	kv := c.Client.KV()
	// 	kv.Delete(c.lockPath()+lockSuffix, nil)
	// 	kv.Delete(c.lockPath()+lockInfoSuffix, nil)

	// 	return nil
	// }

	// // cancel our monitoring goroutine
	// c.monitorCancel()

	// defer func() {
	// 	c.consulLock = nil

	// 	// The consul session is only used for this single lock, so cancel it
	// 	// after we unlock.
	// 	// The session is only created and replaced holding Client.mu, so the
	// 	// CancelFunc must be non-nil.
	// 	c.sessionCancel()
	// }()

	// select {
	// case <-c.lockCh:
	// 	return lostLockErr
	// default:
	// }

	// kv := c.Client.KV()

	// var errs error

	// if _, err := kv.Delete(c.lockPath()+lockInfoSuffix, nil); err != nil {
	// 	errs = multierror.Append(errs, err)
	// }

	// if err := c.consulLock.Unlock(); err != nil {
	// 	errs = multierror.Append(errs, err)
	// }

	// // the monitoring goroutine may be in a select on the lockCh, so we need to
	// // wait for it to return before changing the value.
	// c.monitorWG.Wait()
	// c.lockCh = nil

	// // This is only cleanup, and will fail if the lock was immediately taken by
	// // another client, so we don't report an error to the user here.
	// c.consulLock.Destroy()

	// return errs
}

func compressState(data string) (string, error) {
	b := new(bytes.Buffer)
	gz := gzip.NewWriter(b)
	if _, err := gz.Write([]byte(data)); err != nil {
		return "", err
	}
	if err := gz.Flush(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

func uncompressState(data string) (string, error) {
	b := new(bytes.Buffer)
	reader := strings.NewReader(data)
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return "", err
	}
	b.ReadFrom(gz)
	if err := gz.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

func split(payload string, limit int) []string {
	var chunk string
	chunks := make([]string, 0, len(payload)/limit+1)
	for len(payload) >= limit {
		chunk, payload = payload[:limit], payload[limit:]
		chunks = append(chunks, chunk)
	}
	if len(payload) > 0 {
		chunks = append(chunks, payload[:])
	}
	return chunks
}

func (c *RemoteClient) chunkedMode() (bool, string, []string, *vaultapi.KVSecret, error) {
	kv := c.Client.KVv2(c.Mount)
	ctx := context.TODO()
	secret, err := kv.Get(ctx, c.Name)
	if err != nil {
		return false, "", nil, secret, err
	}
	if secret != nil {
		var d map[string]interface{}

		d = secret.Data
		// If we find the "current-hash" key we were in chunked mode
		hash, ok := d["current-hash"]
		if ok {
			chunks := make([]string, 0)
			for _, c := range d["chunks"].([]interface{}) {
				chunks = append(chunks, c.(string))
			}
			return true, hash.(string), chunks, secret, nil
		}
	}
	return false, "", nil, secret, nil
}
