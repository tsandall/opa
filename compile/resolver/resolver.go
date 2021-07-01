package resolver

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/refactor"
	"github.com/open-policy-agent/opa/util"
)

type Resolver struct {
	ctx   context.Context
	queue []item
}

func New(ctx context.Context) *Resolver {
	return &Resolver{
		ctx: ctx,
	}
}

type item struct {
	url string
	src *ast.Import
	dst *ast.Term
}

func (i *item) RewriteSourceImportPath() {
	i.src.Path = i.dst
	i.src.Alias = ast.Var("")
}

func (l *Resolver) Resolve(opts ast.DependencyResolveOptions) (ast.DependencyResolveResult, error) {

	var result ast.DependencyResolveResult
	result.Result = opts.Modules

	l.enqueue(opts.Modules)
	var head item

	// TODO(tsandall): probably i/o bound; fetch in parallel?
	for len(l.queue) > 0 {

		head, l.queue = l.queue[0], l.queue[1:]
		if _, ok := result.Result[head.url]; ok {
			head.RewriteSourceImportPath()
			continue
		}

		modules, err := l.fetch(head)
		if err != nil {
			return result, err
		}

		// TODO(tsandall): should namespacing use short-common-prefix? This would produce shorter paths in many cases...
		r := refactor.New()
		mr, err := r.Move(refactor.MoveQuery{Modules: modules, SrcDstMapping: map[string]string{"data": head.dst.String()}})
		if err != nil {
			return result, err
		}

		head.RewriteSourceImportPath()
		modules = mr.Result
		l.enqueue(modules)

		for name, module := range modules {
			result.Result[name] = module
		}
	}

	return result, nil
}

func (l *Resolver) enqueue(modules map[string]*ast.Module) {
	for _, module := range modules {
		for _, imp := range module.Imports {
			if url, ok := imp.Path.Value.(ast.String); ok {
				dst := ast.NewTerm(module.Package.Path.Append(ast.StringTerm(string(imp.Alias)))).SetLocation(imp.Path.Loc())
				l.queue = append(l.queue, item{
					url: string(url),
					src: imp,
					dst: dst,
				})
			}
		}
	}
}

// TODO(tsandall): file:// fetching
// TODO(tsandall): caching
// TODO(tsandall): improve error reporting
func (l *Resolver) fetch(x item) (map[string]*ast.Module, error) {

	u, err := url.Parse(x.url)
	if err != nil {
		return nil, err
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported url scheme: %v", u)
	}

	req, err := http.NewRequest("GET", x.url, nil)
	if err != nil {
		return nil, err
	}

	client := defaultRoundTripperClient(&tls.Config{}, 10)
	resp, err := client.Do(req)

	if err != nil {
		return nil, err
	}

	defer util.Close(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %v", resp.Status)
	}

	// TODO(tsandall): add support for bundles, data?
	result := map[string]*ast.Module{}
	mt := getMediaType(resp, "plain/text")

	if _, ok := regoFileMediaTypes[mt]; ok {

		bs, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		module, err := ast.ParseModule(x.url, string(bs))
		if err != nil {
			return nil, err
		}

		result[x.url] = module
	} else {
		return nil, fmt.Errorf("unsupported media type: %v", mt)
	}

	return result, nil
}

// DefaultRoundTripperClient is a reasonable set of defaults for HTTP auth plugins
func defaultRoundTripperClient(t *tls.Config, timeout int64) *http.Client {
	// Ensure we use a http.Transport with proper settings: the zero values are not
	// a good choice, as they cause leaking connections:
	// https://github.com/golang/go/issues/19620

	// copy, we don't want to alter the default client's Transport
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = time.Duration(timeout) * time.Second
	tr.TLSClientConfig = t

	c := *http.DefaultClient
	c.Transport = tr
	return &c
}

func getMediaType(resp *http.Response, fallback string) string {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("content-type"))
	if err != nil {
		return "plain/text"
	}
	return mediaType
}

var regoFileMediaTypes = map[string]struct{}{
	"text/plain":               {},
	"application/octet-stream": {},
}
