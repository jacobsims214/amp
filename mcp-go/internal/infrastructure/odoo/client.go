package odoo

import (
	"context"
	"fmt"
	"github.com/kolo/xmlrpc"
)

// Client represents an Odoo XML-RPC client
type Client struct {
	url      string
	db       string
	username string
	password string
	uid      int
	common   *xmlrpc.Client
	models   *xmlrpc.Client
}

// NewClient creates a new Odoo client
func NewClient(url, db, username, password string) (*Client, error) {
	client := &Client{
		url:      url,
		db:       db,
		username: username,
		password: password,
	}

	if err := client.authenticate(); err != nil {
		return nil, fmt.Errorf("failed to authenticate with Odoo: %w", err)
	}

	return client, nil
}

// authenticate authenticates with Odoo
func (c *Client) authenticate() error {
	common, err := xmlrpc.NewClient(fmt.Sprintf("%s/xmlrpc/2/common", c.url), nil)
	if err != nil {
		return fmt.Errorf("failed to create common client: %w", err)
	}
	c.common = common

	var result interface{}
	if err := c.common.Call("authenticate", []interface{}{c.db, c.username, c.password, map[string]interface{}{}}, &result); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	if result == false {
		return fmt.Errorf("authentication failed: invalid credentials")
	}

	c.uid = int(result.(int64))

	models, err := xmlrpc.NewClient(fmt.Sprintf("%s/xmlrpc/2/object", c.url), nil)
	if err != nil {
		return fmt.Errorf("failed to create models client: %w", err)
	}
	c.models = models

	return nil
}

// Execute calls an Odoo model method with all args as positional arguments and
// no keyword arguments. Use this for: create, write, action_*, message_post.
//
//	execute_kw(db, uid, pw, model, method, [arg1, arg2, ...], {})
func (c *Client) Execute(ctx context.Context, model, method string, args ...interface{}) (interface{}, error) {
	positional := make([]interface{}, len(args))
	copy(positional, args)

	params := []interface{}{c.db, c.uid, c.password, model, method, positional, map[string]interface{}{}}

	var result interface{}
	if err := c.models.Call("execute_kw", params, &result); err != nil {
		return nil, fmt.Errorf("execute failed: %w", err)
	}

	return result, nil
}

// ExecuteKw calls an Odoo model method with positional args and keyword args.
// Use this for: read, search_read, and any method that takes options as kwargs.
//
//	execute_kw(db, uid, pw, model, method, positional, kwargs)
func (c *Client) ExecuteKw(ctx context.Context, model, method string, positional []interface{}, kwargs map[string]interface{}) (interface{}, error) {
	if kwargs == nil {
		kwargs = map[string]interface{}{}
	}

	params := []interface{}{c.db, c.uid, c.password, model, method, positional, kwargs}

	var result interface{}
	if err := c.models.Call("execute_kw", params, &result); err != nil {
		return nil, fmt.Errorf("execute failed: %w", err)
	}

	return result, nil
}

// Version returns the Odoo server version
func (c *Client) Version() (map[string]interface{}, error) {
	var result map[string]interface{}
	if err := c.common.Call("version", []interface{}{}, &result); err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}
	return result, nil
}
