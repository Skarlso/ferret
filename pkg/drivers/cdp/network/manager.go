package network

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"sync"

	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/protocol/fetch"
	"github.com/mafredri/cdp/protocol/network"
	"github.com/mafredri/cdp/protocol/page"
	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/wI2L/jettison"

	"github.com/MontFerret/ferret/pkg/drivers"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/eval"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/events"
	"github.com/MontFerret/ferret/pkg/drivers/cdp/templates"
	"github.com/MontFerret/ferret/pkg/drivers/common"
	"github.com/MontFerret/ferret/pkg/runtime/core"
	"github.com/MontFerret/ferret/pkg/runtime/values"
)

const BlankPageURL = "about:blank"

type (
	FrameLoadedListener = func(ctx context.Context, frame page.Frame)

	Manager struct {
		mu                 sync.Mutex
		logger             *zerolog.Logger
		client             *cdp.Client
		headers            *drivers.HTTPHeaders
		eventLoop          *events.Loop
		cancel             context.CancelFunc
		responseListenerID events.ListenerID
		filterListenerID   events.ListenerID
		response           *sync.Map
	}
)

func New(
	logger *zerolog.Logger,
	client *cdp.Client,
	options Options,
) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())

	m := new(Manager)
	m.logger = logger
	m.client = client
	m.headers = drivers.NewHTTPHeaders()
	m.eventLoop = events.NewLoop()
	m.cancel = cancel
	m.response = new(sync.Map)

	if options.Cookies != nil && len(options.Cookies) > 0 {
		for url, cookies := range options.Cookies {
			if err := m.setCookiesInternal(ctx, url, cookies); err != nil {
				return nil, err
			}
		}
	}

	if options.Headers != nil && options.Headers.Length() > 0 {
		if err := m.setHeadersInternal(ctx, options.Headers); err != nil {
			return nil, err
		}
	}

	var err error

	closers := make([]io.Closer, 0, 10)

	defer func() {
		if err != nil {
			common.CloseAll(logger, closers, "failed to close a DOM event stream")
		}
	}()

	frameNavigatedStream, err := m.client.Page.FrameNavigated(ctx)

	if err != nil {
		return nil, err
	}

	responseReceivedStream, err := m.client.Network.ResponseReceived(ctx)

	if err != nil {
		return nil, err
	}

	m.eventLoop.AddSource(events.NewSource(eventFrameLoad, frameNavigatedStream, func(stream rpcc.Stream) (interface{}, error) {
		return stream.(page.FrameNavigatedClient).Recv()
	}))

	m.eventLoop.AddSource(events.NewSource(responseReceived, responseReceivedStream, func(stream rpcc.Stream) (interface{}, error) {
		return stream.(network.ResponseReceivedClient).Recv()
	}))

	m.responseListenerID = m.eventLoop.AddListener(responseReceived, m.onResponse)

	if options.Filter != nil && len(options.Filter.Patterns) > 0 {
		el2 := events.NewLoop()

		err = m.client.Fetch.Enable(ctx, toFetchArgs(options.Filter.Patterns))

		if err != nil {
			return nil, err
		}

		requestPausedStream, err := m.client.Fetch.RequestPaused(ctx)

		if err != nil {
			return nil, err
		}

		el2.AddSource(events.NewSource(requestPaused, requestPausedStream, func(stream rpcc.Stream) (interface{}, error) {
			return stream.(fetch.RequestPausedClient).Recv()
		}))

		m.filterListenerID = el2.AddListener(requestPaused, m.onRequestPaused)

		// run in a separate loop in order to get higher priority
		// TODO: Consider adding support of event priorities to EventLoop
		el2.Run(ctx)
	}

	m.eventLoop.Run(ctx)

	return m, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	return nil
}

func (m *Manager) GetCookies(ctx context.Context) (*drivers.HTTPCookies, error) {
	repl, err := m.client.Network.GetAllCookies(ctx)

	if err != nil {
		return nil, errors.Wrap(err, "failed to get cookies")
	}

	cookies := drivers.NewHTTPCookies()

	if repl.Cookies == nil {
		return cookies, nil
	}

	for _, c := range repl.Cookies {
		cookies.Set(toDriverCookie(c))
	}

	return cookies, nil
}

func (m *Manager) SetCookies(ctx context.Context, url string, cookies *drivers.HTTPCookies) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.setCookiesInternal(ctx, url, cookies)
}

func (m *Manager) setCookiesInternal(ctx context.Context, url string, cookies *drivers.HTTPCookies) error {
	if cookies == nil {
		return errors.Wrap(core.ErrMissedArgument, "cookies")
	}

	if cookies.Length() == 0 {
		return nil
	}

	params := make([]network.CookieParam, 0, cookies.Length())

	cookies.ForEach(func(value drivers.HTTPCookie, _ values.String) bool {
		params = append(params, fromDriverCookie(url, value))

		return true
	})

	return m.client.Network.SetCookies(ctx, network.NewSetCookiesArgs(params))
}

func (m *Manager) DeleteCookies(ctx context.Context, url string, cookies *drivers.HTTPCookies) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cookies == nil {
		return errors.Wrap(core.ErrMissedArgument, "cookies")
	}

	if cookies.Length() == 0 {
		return nil
	}

	var err error

	cookies.ForEach(func(value drivers.HTTPCookie, _ values.String) bool {
		err = m.client.Network.DeleteCookies(ctx, fromDriverCookieDelete(url, value))

		if err != nil {
			return false
		}

		return true
	})

	return err
}

func (m *Manager) GetHeaders(_ context.Context) (*drivers.HTTPHeaders, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.headers == nil {
		return drivers.NewHTTPHeaders(), nil
	}

	return m.headers.Clone().(*drivers.HTTPHeaders), nil
}

func (m *Manager) SetHeaders(ctx context.Context, headers *drivers.HTTPHeaders) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.setHeadersInternal(ctx, headers)
}

func (m *Manager) setHeadersInternal(ctx context.Context, headers *drivers.HTTPHeaders) error {
	if headers.Length() == 0 {
		return nil
	}

	m.headers = headers

	j, err := jettison.MarshalOpts(headers, jettison.NoHTMLEscaping())

	if err != nil {
		return errors.Wrap(err, "failed to marshal headers")
	}

	err = m.client.Network.SetExtraHTTPHeaders(
		ctx,
		network.NewSetExtraHTTPHeadersArgs(j),
	)

	if err != nil {
		return errors.Wrap(err, "failed to set headers")
	}

	return nil
}

func (m *Manager) GetResponse(_ context.Context, frameID page.FrameID) (drivers.HTTPResponse, error) {
	value, found := m.response.Load(frameID)

	if !found {
		return drivers.HTTPResponse{}, core.ErrNotFound
	}

	return value.(drivers.HTTPResponse), nil
}

func (m *Manager) Navigate(ctx context.Context, url values.String) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if url == "" {
		url = BlankPageURL
	}

	urlStr := url.String()

	repl, err := m.client.Page.Navigate(ctx, page.NewNavigateArgs(urlStr))

	if err != nil {
		return err
	}

	if repl.ErrorText != nil {
		return errors.New(*repl.ErrorText)
	}

	return m.WaitForNavigation(ctx, nil)
}

func (m *Manager) NavigateForward(ctx context.Context, skip values.Int) (values.Boolean, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	history, err := m.client.Page.GetNavigationHistory(ctx)

	if err != nil {
		return values.False, err
	}

	length := len(history.Entries)
	lastIndex := length - 1

	// nowhere to go forward
	if history.CurrentIndex == lastIndex {
		return values.False, nil
	}

	if skip < 1 {
		skip = 1
	}

	to := int(skip) + history.CurrentIndex

	if to > lastIndex {
		// TODO: Return error?
		return values.False, nil
	}

	entry := history.Entries[to]
	err = m.client.Page.NavigateToHistoryEntry(ctx, page.NewNavigateToHistoryEntryArgs(entry.ID))

	if err != nil {
		return values.False, err
	}

	err = m.WaitForNavigation(ctx, nil)

	if err != nil {
		return values.False, err
	}

	return values.True, nil
}

func (m *Manager) NavigateBack(ctx context.Context, skip values.Int) (values.Boolean, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	history, err := m.client.Page.GetNavigationHistory(ctx)

	if err != nil {
		return values.False, err
	}

	// we are in the beginning
	if history.CurrentIndex == 0 {
		return values.False, nil
	}

	if skip < 1 {
		skip = 1
	}

	to := history.CurrentIndex - int(skip)

	if to < 0 {
		// TODO: Return error?
		return values.False, nil
	}

	entry := history.Entries[to]
	err = m.client.Page.NavigateToHistoryEntry(ctx, page.NewNavigateToHistoryEntryArgs(entry.ID))

	if err != nil {
		return values.False, err
	}

	err = m.WaitForNavigation(ctx, nil)

	if err != nil {
		return values.False, err
	}

	return values.True, nil
}

func (m *Manager) WaitForNavigation(ctx context.Context, pattern *regexp.Regexp) error {
	return m.WaitForFrameNavigation(ctx, "", pattern)
}

func (m *Manager) WaitForFrameNavigation(ctx context.Context, frameID page.FrameID, urlPattern *regexp.Regexp) error {
	onEvent := make(chan struct{})

	m.eventLoop.AddListener(eventFrameLoad, func(_ context.Context, message interface{}) bool {
		repl := message.(*page.FrameNavigatedReply)

		var matched bool

		// if frameID is empty string or equals to the current one
		if len(frameID) == 0 || repl.Frame.ID == frameID {
			// if a URL pattern is provided
			if urlPattern != nil {
				matched = urlPattern.Match([]byte(repl.Frame.URL))
			} else {
				// otherwise just notify
				matched = true
			}
		}

		if matched {
			if ctx.Err() == nil {
				ec, err := eval.NewExecutionContextFrom(ctx, m.client, repl.Frame)

				if err != nil {
					close(onEvent)
					return false
				}

				_, err = events.NewEvalWaitTask(
					ec,
					templates.DOMReady(),
					events.DefaultPolling,
				).Run(ctx)

				if err != nil {
					close(onEvent)
					return false
				}

				onEvent <- struct{}{}
				close(onEvent)
			}
		}

		// if not matched - continue listening
		return !matched
	})

	select {
	case <-onEvent:
		return nil
	case <-ctx.Done():
		return core.ErrTimeout
	}
}

func (m *Manager) AddFrameLoadedListener(listener FrameLoadedListener) events.ListenerID {
	return m.eventLoop.AddListener(eventFrameLoad, func(ctx context.Context, message interface{}) bool {
		repl := message.(*page.FrameNavigatedReply)

		listener(ctx, repl.Frame)

		return true
	})
}

func (m *Manager) RemoveFrameLoadedListener(id events.ListenerID) {
	m.eventLoop.RemoveListener(eventFrameLoad, id)
}

func (m *Manager) onResponse(_ context.Context, message interface{}) (out bool) {
	out = true
	msg, ok := message.(*network.ResponseReceivedReply)

	if !ok {
		return
	}

	// we are interested in documents only
	if msg.Type != network.ResourceTypeDocument {
		return
	}

	response := drivers.HTTPResponse{
		StatusCode: msg.Response.Status,
		Status:     msg.Response.StatusText,
		Headers:    drivers.NewHTTPHeaders(),
	}

	deserialized := make(map[string]string)

	if len(msg.Response.Headers) > 0 {
		err := json.Unmarshal(msg.Response.Headers, &deserialized)

		if err != nil {
			m.logger.Error().Err(err).Msg("failed to deserialize response headers")
		}
	}

	for key, value := range deserialized {
		response.Headers.Set(key, value)
	}

	m.response.Store(*msg.FrameID, response)

	return
}

func (m *Manager) onRequestPaused(ctx context.Context, message interface{}) (out bool) {
	out = true
	msg, ok := message.(*fetch.RequestPausedReply)

	if !ok {
		return
	}

	err := m.client.Fetch.FailRequest(ctx, &fetch.FailRequestArgs{
		RequestID:   msg.RequestID,
		ErrorReason: network.ErrorReasonBlockedByClient,
	})

	if err != nil {
		m.logger.
			Err(err).
			Str("resourceType", msg.ResourceType.String()).
			Str("url", msg.Request.URL).
			Msg("failed to terminate a request")
	}

	return
}
