/*
Copyright 2011 Google Inc.

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

package search

import (
	"fmt"
	"http"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"camli/blobref"
	"camli/blobserver"
	"camli/jsonconfig"
	"camli/httputil"
)

func init() {
	blobserver.RegisterHandlerConstructor("search", newHandlerFromConfig)
}

type Handler struct {
	index Index
	owner *blobref.BlobRef
}

func newHandlerFromConfig(ld blobserver.Loader, conf jsonconfig.Obj) (http.Handler, os.Error) {
	indexPrefix := conf.RequiredString("index") // TODO: add optional help tips here?
	ownerBlobStr := conf.RequiredString("owner")
	if err := conf.Validate(); err != nil {
		return nil, err
	}

	indexHandler, err := ld.GetHandler(indexPrefix)
	if err != nil {
		return nil, fmt.Errorf("search config references unknown handler %q", indexPrefix)
	}
	indexer, ok := indexHandler.(Index)
	if !ok {
		return nil, fmt.Errorf("search config references invalid indexer %q (actually a %T)", indexPrefix, indexHandler)
	}
	ownerBlobRef := blobref.Parse(ownerBlobStr)
	if ownerBlobRef == nil {
		return nil, fmt.Errorf("search 'owner' has malformed blobref %q; expecting e.g. sha1-xxxxxxxxxxxx",
			ownerBlobStr)
	}
	return &Handler{
		index: indexer,
		owner: ownerBlobRef,
	}, nil
}


// TODO: figure out a plan for an owner having multiple active public keys, or public
// key rotation
func (h *Handler) Owner() *blobref.BlobRef {
	return h.owner
}

func (h *Handler) Index() Index {
	return h.index
}

func jsonMap() map[string]interface{} {
	return make(map[string]interface{})
}

func jsonMapList() []map[string]interface{} {
	return make([]map[string]interface{}, 0)
}

func (sh *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()
	_ = req.Header.Get("X-PrefixHandler-PathBase")
	suffix := req.Header.Get("X-PrefixHandler-PathSuffix")

	if req.Method == "GET" {
		switch suffix {
		case "camli/search/recent":
			sh.serveRecentPermanodes(rw, req)
			return
		case "camli/search/describe":
			sh.serveDescribe(rw, req)
			return
		case "camli/search/claims":
			sh.serveClaims(rw, req)
			return
		case "camli/search/files":
			sh.serveFiles(rw, req)
			return
		case "camli/search/signerattrvalue":
			sh.serveSignerAttrValue(rw, req)
			return
		case "camli/search/signerpaths":
			sh.serveSignerPaths(rw, req)
			return
		}
	}

	// TODO: discovery for the endpoints & better error message with link to discovery info
	ret["error"] = "Unsupported search path or method"
	ret["errorType"] = "input"
	httputil.ReturnJson(rw, ret)
}

func (sh *Handler) serveRecentPermanodes(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()
	defer httputil.ReturnJson(rw, ret)

	ch := make(chan *Result)
	errch := make(chan os.Error)
	go func() {
		errch <- sh.index.GetRecentPermanodes(ch, []*blobref.BlobRef{sh.owner}, 50)
	}()

	dr := sh.NewDescribeRequest()

	recent := jsonMapList()
	for res := range ch {
		dr.Describe(res.BlobRef, 2)
		jm := jsonMap()
		jm["blobref"] = res.BlobRef.String()
		jm["owner"] = res.Signer.String()
		t := time.SecondsToUTC(res.LastModTime)
		jm["modtime"] = t.Format(time.RFC3339)
		recent = append(recent, jm)
	}

	err := <-errch
	if err != nil {
		// TODO: return error status code
		ret["error"] = fmt.Sprintf("%v", err)
		return
	}

	ret["recent"] = recent
	dr.PopulateJSON(ret)
}

func (sh *Handler) serveClaims(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()

	pn := blobref.Parse(req.FormValue("permanode"))
	if pn == nil {
		http.Error(rw, "Missing or invalid 'permanode' param", 400)
		return
	}

	// TODO: rename GetOwnerClaims to GetClaims?
	claims, err := sh.index.GetOwnerClaims(pn, sh.owner)
	if err != nil {
		log.Printf("Error getting claims of %s: %v", pn.String(), err)
	} else {
		sort.Sort(claims)
		jclaims := jsonMapList()

		for _, claim := range claims {
			jclaim := jsonMap()
			jclaim["blobref"] = claim.BlobRef.String()
			jclaim["signer"] = claim.Signer.String()
			jclaim["permanode"] = claim.Permanode.String()
			jclaim["date"] = claim.Date.Format(time.RFC3339)
			jclaim["type"] = claim.Type
			if claim.Attr != "" {
				jclaim["attr"] = claim.Attr
			}
			if claim.Value != "" {
				jclaim["value"] = claim.Value
			}

			jclaims = append(jclaims, jclaim)
		}
		ret["claims"] = jclaims
	}

	httputil.ReturnJson(rw, ret)
}

type DescribeRequest struct {
	sh *Handler

	lk   sync.Mutex // protects following:
	m    map[string]*DescribedBlob
	done map[string]bool     // blobref -> described
	errs map[string]os.Error // blobref -> error

	wg *sync.WaitGroup // for load requests
}

type DescribedBlob struct {
	BlobRef   *blobref.BlobRef
	MimeType  string
	CamliType string
	// TODO: just int is probably fine, if we're going to be capping blobs at 32MB?
	Size int64

	// if camliType "permanode"
	Permanode *DescribedPermanode

	// if camliType "file"
	File *FileInfo
}

func (b *DescribedBlob) jsonMap() map[string]interface{} {
	m := jsonMap()
	m["blobRef"] = b.BlobRef.String()
	if b.MimeType != "" {
		m["mimeType"] = b.MimeType
	}
	if b.CamliType != "" {
		m["camliType"] = b.CamliType
	}
	m["size"] = b.Size
	if b.Permanode != nil {
		m["permanode"] = b.Permanode.jsonMap()
	}
	if b.File != nil {
		m["file"] = b.File
	}
	return m
}

type DescribedPermanode struct {
	Attr map[string][]string
}

func (dp *DescribedPermanode) jsonMap() map[string]interface{} {
	m := jsonMap()

	am := jsonMap()
	m["attr"] = am
	for k, vv := range dp.Attr {
		if len(vv) > 0 {
			vl := make([]string, len(vv))
			copy(vl[:], vv[:])
			am[k] = vl
		}
	}
	return m
}

// NewDescribeRequest returns a new DescribeRequest holding the state
// of blobs and their summarized descriptions.  Use DescribeBlob
// one or more times before calling PopulateJSON or Result.
func (sh *Handler) NewDescribeRequest() *DescribeRequest {
	return &DescribeRequest{
		sh:   sh,
		m:    make(map[string]*DescribedBlob),
		errs: make(map[string]os.Error),
		wg:   new(sync.WaitGroup),
	}
}

type DescribeError map[string]os.Error

func (e DescribeError) String() string {
	return "one or more errors describing blobs"
}

// Result waits for all outstanding lookups to complete and
// returns the map of blobref (strings) to their described
// results. The returned error is non-nil if any errors
// occured.
func (dr *DescribeRequest) Result() (map[string]*DescribedBlob, DescribeError) {
	dr.wg.Wait()
	// TODO: set "done" / locked flag, so no more DescribeBlob can
	// be called.
	var err DescribeError
	if len(dr.errs) > 0 {
		err = DescribeError(dr.errs)
	}
	return dr.m, err
}

// PopulateJSON waits for all outstanding lookups to complete and populates
// the results into the provided dest map, suitable for marshalling
// as JSON with the json package.
func (dr *DescribeRequest) PopulateJSON(dest map[string]interface{}) {
	dr.wg.Wait()
	dr.lk.Lock()
	defer dr.lk.Unlock()
	for k, v := range dr.m {
		dest[k] = v.jsonMap()
	}
	for k, err := range dr.errs {
		dest["error"] = "error populating " + k + ": " + err.String()
		break // TODO: include all?
	}
}

func (dr *DescribeRequest) describedBlob(b *blobref.BlobRef) *DescribedBlob {
	dr.lk.Lock()
	defer dr.lk.Unlock()
	bs := b.String()
	if des, ok := dr.m[bs]; ok {
		return des
	}
	des := &DescribedBlob{BlobRef: b}
	dr.m[bs] = des
	return des
}

func (dr *DescribeRequest) Describe(br *blobref.BlobRef, depth int) {
	if depth <= 0 {
		return
	}
	dr.lk.Lock()
	defer dr.lk.Unlock()
	if dr.done == nil {
		dr.done = make(map[string]bool)
	}
	if dr.done[br.String()] {
		return
	}
	dr.done[br.String()] = true
	dr.wg.Add(1)
	go func() {
		defer dr.wg.Done()
		dr.describeReally(br, depth)
	}()
}

func (dr *DescribeRequest) addError(br *blobref.BlobRef, err os.Error) {
	if err == nil {
		return
	}
	dr.lk.Lock()
	defer dr.lk.Unlock()
	// TODO: append? meh.
	dr.errs[br.String()] = err
}

func (dr *DescribeRequest) describeReally(br *blobref.BlobRef, depth int) {
	mime, size, err := dr.sh.index.GetBlobMimeType(br)
	if err == os.ENOENT {
		return
	}
	if err != nil {
		dr.addError(br, err)
		return
	}

	// TODO: convert all this in terms of
	// DescribedBlob/DescribedPermanode/DescribedFile, not json
	// maps.  Then add JSON marhsallers to those types. Add tests.
	des := dr.describedBlob(br)
	des.setMimeType(mime)
	des.Size = size

	switch des.CamliType {
	case "permanode":
		des.Permanode = new(DescribedPermanode)
		dr.populatePermanodeFields(des.Permanode, br, dr.sh.owner, depth)
	case "file":
		var err os.Error
		des.File, err = dr.sh.index.GetFileInfo(br)
		if err != nil {
			dr.addError(br, err)
		}
	}
}

func (sh *Handler) serveDescribe(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()
	defer httputil.ReturnJson(rw, ret)

	br := blobref.Parse(req.FormValue("blobref"))
	if br == nil {
		ret["error"] = "Missing or invalid 'blobref' param"
		ret["errorType"] = "input"
		return
	}

	dr := sh.NewDescribeRequest()
	dr.Describe(br, 4)
	dr.PopulateJSON(ret)
}

func (sh *Handler) serveFiles(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()
	defer httputil.ReturnJson(rw, ret)

	br := blobref.Parse(req.FormValue("bytesref"))
	if br == nil {
		// TODO: formalize how errors are returned And make
		// ReturnJson set the HTTP status to 400 automatically
		// in some cases, if errorType is "input"?  Document
		// this somewhere.  Are there existing JSON
		// conventions to use?
		ret["error"] = "Missing or invalid 'bytesref' param"
		ret["errorType"] = "input"
		return
	}

	files, err := sh.index.ExistingFileSchemas(br)
	if err != nil {
		ret["error"] = err.String()
		ret["errorType"] = "server"
		return
	}

	strList := []string{}
	for _, br := range files {
		strList = append(strList, br.String())
	}
	ret["files"] = strList
	return
}

func (dr *DescribeRequest) populatePermanodeFields(pi *DescribedPermanode, pn, signer *blobref.BlobRef, depth int) {
	pi.Attr = make(map[string][]string)
	attr := pi.Attr

	claims, err := dr.sh.index.GetOwnerClaims(pn, signer)
	if err != nil {
		log.Printf("Error getting claims of %s: %v", pn.String(), err)
		dr.addError(pn, fmt.Errorf("Error getting claims of %s: %v", pn.String(), err))
		return
	}

	sort.Sort(claims)
claimLoop:
	for _, cl := range claims {
		switch cl.Type {
		case "del-attribute":
			if cl.Value == "" {
				attr[cl.Attr] = nil, false
			} else {
				sl := attr[cl.Attr]
				filtered := make([]string, 0, len(sl))
				for _, val := range sl {
					if val != cl.Value {
						filtered = append(filtered, val)
					}
				}
				attr[cl.Attr] = filtered
			}
		case "set-attribute":
			attr[cl.Attr] = nil, false
			fallthrough
		case "add-attribute":
			if cl.Value == "" {
				continue
			}
			sl, ok := attr[cl.Attr]
			if ok {
				for _, exist := range sl {
					if exist == cl.Value {
						continue claimLoop
					}
				}
			} else {
				sl = make([]string, 0, 1)
				attr[cl.Attr] = sl
			}
			attr[cl.Attr] = append(sl, cl.Value)
		}
	}

	// If the content permanode is now known, look up its type
	if content, ok := attr["camliContent"]; ok && len(content) > 0 {
		cbr := blobref.Parse(content[len(content)-1])
		dr.Describe(cbr, depth-1)
	}

	// Resolve children
	if members, ok := attr["camliMember"]; ok {
		for _, member := range members {
			membr := blobref.Parse(member)
			if membr != nil {
				dr.Describe(membr, depth-1)
			}
		}
	}
}

func mustGet(req *http.Request, param string) string {
	v := req.FormValue(param)
	if v == "" {
		panic(fmt.Sprintf("missing required parameter %q", param))
	}
	return v
}

func setPanicError(m map[string]interface{}) {
	p := recover()
	if p == nil {
		return
	}
	m["error"] = p.(string)
	m["errorType"] = "input"
}

func (sh *Handler) serveSignerAttrValue(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()
	defer httputil.ReturnJson(rw, ret)
	defer setPanicError(ret)

	signer := blobref.MustParse(mustGet(req, "signer"))
	attr := mustGet(req, "attr")
	value := mustGet(req, "value")
	pn, err := sh.index.PermanodeOfSignerAttrValue(signer, attr, value)
	if err != nil {
		ret["error"] = err.String()
	} else {
		ret["permanode"] = pn.String()

		dr := sh.NewDescribeRequest()
		dr.Describe(pn, 2)
		dr.PopulateJSON(ret)
	}
}

func (sh *Handler) serveSignerPaths(rw http.ResponseWriter, req *http.Request) {
	ret := jsonMap()
	defer httputil.ReturnJson(rw, ret)
	defer setPanicError(ret)

	signer := blobref.MustParse(mustGet(req, "signer"))
	target := blobref.MustParse(mustGet(req, "target"))
	paths, err := sh.index.PathsOfSignerTarget(signer, target)
	if err != nil {
		ret["error"] = err.String()
	} else {
		jpaths := []map[string]interface{}{}
		for _, path := range paths {
			jpaths = append(jpaths, map[string]interface{}{
				"claimRef": path.Claim.String(),
				"baseRef":  path.Base.String(),
				"suffix":   path.Suffix,
			})
		}
		ret["paths"] = jpaths
		dr := sh.NewDescribeRequest()
		for _, path := range paths {
			dr.Describe(path.Base, 2)
		}
		dr.PopulateJSON(ret)
	}
}

const camliTypePrefix = "application/json; camliType="

func (d *DescribedBlob) setMimeType(mime string) {
	d.MimeType = mime
	if strings.HasPrefix(mime, camliTypePrefix) {
		d.CamliType = mime[len(camliTypePrefix):]
	}
}
