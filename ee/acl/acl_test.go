// +build !oss

/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Dgraph Community License (the "License"); you
 * may not use this file except in compliance with the License. You
 * may obtain a copy of the License at
 *
 *     https://github.com/dgraph-io/dgraph/blob/master/licenses/DCL.txt
 */

package acl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v2"
	"github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/dgraph-io/dgraph/testutil"
	"github.com/dgraph-io/dgraph/x"
	"github.com/golang/glog"
	"github.com/stretchr/testify/require"
)

var (
	userid         = "alice"
	userpassword   = "simplepassword"
	dgraphEndpoint = testutil.SockAddr
)

func checkOutput(t *testing.T, cmd *exec.Cmd, shouldFail bool) string {
	out, err := cmd.CombinedOutput()
	if (!shouldFail && err != nil) || (shouldFail && err == nil) {
		t.Errorf("Error output from command:%v", string(out))
		t.Fatal(err)
	}

	return string(out)
}

func createUser(t *testing.T, accessToken, username, password string) []byte {
	addUser := `mutation addUser($name: String!, $pass: String!) {
		addUser(input: [{name: $name, password: $pass}]) {
			user {
				name
			}
		}
	}`

	params := testutil.GraphQLParams{
		Query: addUser,
		Variables: map[string]interface{}{
			"name": username,
			"pass": password,
		},
	}
	b := makeRequest(t, accessToken, params)
	return b
}

func checkUserCount(t *testing.T, resp []byte, expected int) {
	type Response struct {
		Data struct {
			AddUser struct {
				User []struct {
					Name string
				}
			}
		}
	}

	var r Response
	err := json.Unmarshal(resp, &r)
	require.NoError(t, err)
	require.Equal(t, expected, len(r.Data.AddUser.User))
}

func deleteUser(t *testing.T, accessToken, username string) {
	// TODO - Verify that only one uid got deleted once numUids are returned as part of the payload.
	delUser := `mutation deleteUser($name: String!) {
		deleteUser(filter: {name: {eq: $name}}) {
			msg
		}
	}`

	params := testutil.GraphQLParams{
		Query: delUser,
		Variables: map[string]interface{}{
			"name": username,
		},
	}
	b := makeRequest(t, accessToken, params)
	require.JSONEq(t, `{"data":{"deleteUser":{"msg":"Deleted"}}}`, string(b))
}

func TestCreateAndDeleteUsers(t *testing.T) {
	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")

	// clean up the user to allow repeated running of this test
	deleteUser(t, accessJwt, userid)
	glog.Infof("cleaned up db user state")

	resp := createUser(t, accessJwt, userid, userpassword)
	checkUserCount(t, resp, 1)

	// adding the user again should fail
	resp = createUser(t, accessJwt, userid, userpassword)
	checkUserCount(t, resp, 0)

	// delete the user
	deleteUser(t, accessJwt, userid)

	resp = createUser(t, accessJwt, userid, userpassword)
	// now we should be able to create the user again
	checkUserCount(t, resp, 1)
}

func resetUser(t *testing.T) {
	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")

	// clean up the user to allow repeated running of this test
	deleteUser(t, accessJwt, userid)
	glog.Infof("deleted user")

	resp := createUser(t, accessJwt, userid, userpassword)
	checkUserCount(t, resp, 1)
	glog.Infof("created user")
}

func TestReservedPredicates(t *testing.T) {
	// This test uses the groot account to ensure that reserved predicates
	// cannot be altered even if the permissions allow it.
	ctx := context.Background()

	dg1, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	alterReservedPredicates(t, dg1)

	dg2, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	if err := dg2.Login(ctx, x.GrootId, "password"); err != nil {
		t.Fatalf("unable to login using the groot account:%v", err)
	}
	alterReservedPredicates(t, dg2)
}

func TestAuthorization(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping because -short=true")
	}

	glog.Infof("testing with port 9180")
	dg1, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	testAuthorization(t, dg1)
	glog.Infof("done")

	glog.Infof("testing with port 9182")
	dg2, err := testutil.DgraphClientWithGroot(":9182")
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	testAuthorization(t, dg2)
	glog.Infof("done")
}

func testAuthorization(t *testing.T, dg *dgo.Dgraph) {
	createAccountAndData(t, dg)
	ctx := context.Background()
	if err := dg.Login(ctx, userid, userpassword); err != nil {
		t.Fatalf("unable to login using the account %v", userid)
	}

	// initially the query should return empty result, mutate and alter
	// operations should all fail when there are no rules defined on the predicates
	queryPredicateWithUserAccount(t, dg, false)
	mutatePredicateWithUserAccount(t, dg, true)
	alterPredicateWithUserAccount(t, dg, true)
	createGroupAndAcls(t, unusedGroup, false)
	// wait for 6 seconds to ensure the new acl have reached all acl caches
	glog.Infof("Sleeping for 6 seconds for acl caches to be refreshed")
	time.Sleep(6 * time.Second)

	// now all these operations except query should fail since
	// there are rules defined on the unusedGroup
	queryPredicateWithUserAccount(t, dg, false)
	mutatePredicateWithUserAccount(t, dg, true)
	alterPredicateWithUserAccount(t, dg, true)
	// create the dev group and add the user to it
	createGroupAndAcls(t, devGroup, true)

	// wait for 6 seconds to ensure the new acl have reached all acl caches
	glog.Infof("Sleeping for 6 seconds for acl caches to be refreshed")
	time.Sleep(6 * time.Second)

	// now the operations should succeed again through the devGroup
	queryPredicateWithUserAccount(t, dg, false)
	// sleep long enough (10s per the docker-compose.yml)
	// for the accessJwt to expire in order to test auto login through refresh jwt
	glog.Infof("Sleeping for 4 seconds for accessJwt to expire")
	time.Sleep(4 * time.Second)
	mutatePredicateWithUserAccount(t, dg, false)
	glog.Infof("Sleeping for 4 seconds for accessJwt to expire")
	time.Sleep(4 * time.Second)
	alterPredicateWithUserAccount(t, dg, false)
}

var predicateToRead = "predicate_to_read"
var queryAttr = "name"
var predicateToWrite = "predicate_to_write"
var predicateToAlter = "predicate_to_alter"
var devGroup = "dev"
var unusedGroup = "unusedGroup"
var query = fmt.Sprintf(`
	{
		q(func: eq(%s, "SF")) {
			%s
		}
	}`, predicateToRead, queryAttr)
var schemaQuery = "schema {}"

func alterReservedPredicates(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()

	// Test that alter requests are allowed if the new update is the same as
	// the initial update for a reserved predicate.
	err := dg.Alter(ctx, &api.Operation{
		Schema: "dgraph.xid: string @index(exact) @upsert .",
	})
	require.NoError(t, err)

	err = dg.Alter(ctx, &api.Operation{
		Schema: "dgraph.xid: int .",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(),
		"predicate dgraph.xid is reserved and is not allowed to be modified")

	err = dg.Alter(ctx, &api.Operation{
		DropAttr: "dgraph.xid",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(),
		"predicate dgraph.xid is reserved and is not allowed to be dropped")

	// Test that reserved predicates act as case-insensitive.
	err = dg.Alter(ctx, &api.Operation{
		Schema: "dgraph.XID: int .",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(),
		"predicate dgraph.XID is reserved and is not allowed to be modified")
}

func queryPredicateWithUserAccount(t *testing.T, dg *dgo.Dgraph, shouldFail bool) {
	ctx := context.Background()
	txn := dg.NewTxn()
	_, err := txn.Query(ctx, query)
	if shouldFail {
		require.Error(t, err, "the query should have failed")
	} else {
		require.NoError(t, err, "the query should have succeeded")
	}
}

func querySchemaWithUserAccount(t *testing.T, dg *dgo.Dgraph, shouldFail bool) {
	ctx := context.Background()
	txn := dg.NewTxn()
	_, err := txn.Query(ctx, schemaQuery)

	if shouldFail {
		require.Error(t, err, "the query should have failed")
	} else {
		require.NoError(t, err, "the query should have succeeded")
	}
}

func mutatePredicateWithUserAccount(t *testing.T, dg *dgo.Dgraph, shouldFail bool) {
	ctx := context.Background()
	txn := dg.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`_:a <%s>  "string" .`, predicateToWrite)),
	})

	if shouldFail {
		require.Error(t, err, "the mutation should have failed")
	} else {
		require.NoError(t, err, "the mutation should have succeeded")
	}
}

func alterPredicateWithUserAccount(t *testing.T, dg *dgo.Dgraph, shouldFail bool) {
	ctx := context.Background()
	err := dg.Alter(ctx, &api.Operation{
		Schema: fmt.Sprintf(`%s: int .`, predicateToAlter),
	})
	if shouldFail {
		require.Error(t, err, "the alter should have failed")
	} else {
		require.NoError(t, err, "the alter should have succeeded")
	}
}

func createAccountAndData(t *testing.T, dg *dgo.Dgraph) {
	// use the groot account to clean the database
	ctx := context.Background()
	if err := dg.Login(ctx, x.GrootId, "password"); err != nil {
		t.Fatalf("unable to login using the groot account:%v", err)
	}
	op := api.Operation{
		DropAll: true,
	}
	if err := dg.Alter(ctx, &op); err != nil {
		t.Fatalf("Unable to cleanup db:%v", err)
	}
	require.NoError(t, dg.Alter(ctx, &api.Operation{
		Schema: fmt.Sprintf(`%s: string @index(exact) .`, predicateToRead),
	}))
	// wait for 6 seconds to ensure the new acl have reached all acl caches
	glog.Infof("Sleeping for 6 seconds for acl caches to be refreshed")
	time.Sleep(6 * time.Second)

	// create some data, e.g. user with name alice
	resetUser(t)

	txn := dg.NewTxn()
	_, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(fmt.Sprintf("_:a <%s> \"SF\" .", predicateToRead)),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))
}

func createGroup(t *testing.T, accessToken, name string) []byte {
	addGroup := `mutation addGroup($name: String!) {
		addGroup(input: [{name: $name}]) {
			group {
				name
			}
		}
	}`

	params := testutil.GraphQLParams{
		Query: addGroup,
		Variables: map[string]interface{}{
			"name": name,
		},
	}
	b := makeRequest(t, accessToken, params)
	return b
}

func checkGroupCount(t *testing.T, resp []byte, expected int) {
	type Response struct {
		Data struct {
			AddGroup struct {
				Group []struct {
					Name string
				}
			}
		}
	}

	var r Response
	err := json.Unmarshal(resp, &r)
	require.NoError(t, err)
	require.Equal(t, expected, len(r.Data.AddGroup.Group))
}

func addToGroup(t *testing.T, accessToken, userId, group string) {
	addUserToGroup := `mutation updateUser($name: String!, $group: String!) {
		updateUser(input: {
			filter: {
				name: {
					eq: $name
				}
			},
			set: {
				groups: [
					{ name: $group }
				]
			}
		}) {
			user {
				name
				groups {
					name
				}
			}
		}
	}`

	params := testutil.GraphQLParams{
		Query: addUserToGroup,
		Variables: map[string]interface{}{
			"name":  userId,
			"group": group,
		},
	}
	b := makeRequest(t, accessToken, params)
	expectedOutput := fmt.Sprintf(`{"data":{"updateUser":{"user":[{"name":"%s","groups":[{"name":"%s"}]}]}}}`,
		userId, group)
	require.JSONEq(t, expectedOutput, string(b))
}

type rule struct {
	Predicate  string `json:"predicate"`
	Permission int32  `json:"permission"`
}

func makeRequest(t *testing.T, accessToken string, params testutil.GraphQLParams) []byte {
	adminUrl := "http://" + testutil.SockAddrHttp + "/admin"

	b, err := json.Marshal(params)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, adminUrl, bytes.NewBuffer(b))
	require.NoError(t, err)
	req.Header.Set("X-Dgraph-AccessToken", accessToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()
	b, err = ioutil.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

func addRulesToGroup(t *testing.T, accessToken, group string, rules []rule) {
	addRuleToGroup := `mutation updateGroup($name: String!, $rules: [RuleRef]) {
		updateGroup(input: {
			filter: {
				name: {
					eq: $name
				}
			},
			set: {
				rules: $rules
			}
		}) {
			group {
				name
				rules {
					predicate
					permission
				}
			}
		}
	}`

	params := testutil.GraphQLParams{
		Query: addRuleToGroup,
		Variables: map[string]interface{}{
			"name":  group,
			"rules": rules,
		},
	}
	b := makeRequest(t, accessToken, params)
	rulesb, err := json.Marshal(rules)
	require.NoError(t, err)
	expectedOutput := fmt.Sprintf(`{
		"data": {
		  "updateGroup": {
			"group": [
			  {
				"name": "%s",
				"rules": %s
			  }
			]
		  }
		}
	  }`, group, rulesb)
	testutil.CompareJSON(t, expectedOutput, string(b))
}

func createGroupAndAcls(t *testing.T, group string, addUserToGroup bool) {
	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")

	// create a new group
	resp := createGroup(t, accessJwt, group)
	checkGroupCount(t, resp, 1)

	// add the user to the group
	if addUserToGroup {
		addToGroup(t, accessJwt, userid, group)
	}

	rules := []rule{
		{
			predicateToRead, Read.Code,
		},
		{
			queryAttr, Read.Code,
		},
		{
			predicateToWrite, Write.Code,
		},
		{
			predicateToAlter, Modify.Code,
		},
	}

	// add READ permission on the predicateToRead to the group
	// also add read permission to the attribute queryAttr, which is used inside the query block
	// add WRITE permission on the predicateToWrite
	// add MODIFY permission on the predicateToAlter
	addRulesToGroup(t, accessJwt, group, rules)
}

func TestPredicatePermission(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping because -short=true")
	}

	glog.Infof("testing with port 9180")
	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	if err != nil {
		t.Fatalf("Error while getting a dgraph client: %v", err)
	}
	createAccountAndData(t, dg)
	ctx := context.Background()
	err = dg.Login(ctx, userid, userpassword)
	require.NoError(t, err, "Logging in with the current password should have succeeded")

	// Schema query is allowed to all logged in users.
	querySchemaWithUserAccount(t, dg, false)

	// The query should return emptry response, alter and mutation
	// should be blocked when no rule is defined.
	queryPredicateWithUserAccount(t, dg, false)
	mutatePredicateWithUserAccount(t, dg, true)
	alterPredicateWithUserAccount(t, dg, true)
	createGroupAndAcls(t, unusedGroup, false)

	// Wait for 6 seconds to ensure the new acl have reached all acl caches.
	glog.Infof("Sleeping for 6 seconds for acl caches to be refreshed")
	time.Sleep(6 * time.Second)
	// The operations except query should fail when there is a rule defined, but the
	// current user is not allowed.
	queryPredicateWithUserAccount(t, dg, false)
	mutatePredicateWithUserAccount(t, dg, true)
	alterPredicateWithUserAccount(t, dg, true)
	// Schema queries should still succeed since they are not tied to specific predicates.
	querySchemaWithUserAccount(t, dg, false)
}

func TestAccessWithoutLoggingIn(t *testing.T) {
	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)

	createAccountAndData(t, dg)
	dg, err = testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err)

	// Without logging in, the anonymous user should be evaluated as if the user does not
	// belong to any group, and access should not be granted if there is no ACL rule defined
	// for a predicate.
	queryPredicateWithUserAccount(t, dg, true)
	mutatePredicateWithUserAccount(t, dg, true)
	alterPredicateWithUserAccount(t, dg, true)

	// Schema queries should fail if the user has not logged in.
	querySchemaWithUserAccount(t, dg, true)
}

func TestUnauthorizedDeletion(t *testing.T) {
	ctx, _ := context.WithTimeout(context.Background(), 100*time.Second)
	unAuthPred := "unauthorizedPredicate"

	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)

	op := api.Operation{
		DropAll: true,
	}
	require.NoError(t, dg.Alter(ctx, &op))

	op = api.Operation{
		Schema: fmt.Sprintf("%s: string @index(exact) .", unAuthPred),
	}
	require.NoError(t, dg.Alter(ctx, &op))

	resetUser(t)

	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")
	createGroup(t, accessJwt, devGroup)

	addToGroup(t, accessJwt, userid, devGroup)

	txn := dg.NewTxn()
	mutation := &api.Mutation{
		SetNquads: []byte(fmt.Sprintf("_:a <%s> \"testdata\" .", unAuthPred)),
		CommitNow: true,
	}
	resp, err := txn.Mutate(ctx, mutation)
	require.NoError(t, err)

	nodeUID, ok := resp.Uids["a"]
	require.True(t, ok)

	addRulesToGroup(t, accessJwt, devGroup, []rule{{unAuthPred, 0}})

	userClient, err := testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err)
	time.Sleep(6 * time.Second)

	err = userClient.Login(ctx, userid, userpassword)
	require.NoError(t, err)

	txn = userClient.NewTxn()
	mutString := fmt.Sprintf("<%s> <%s> * .", nodeUID, unAuthPred)
	mutation = &api.Mutation{
		DelNquads: []byte(mutString),
		CommitNow: true,
	}
	_, err = txn.Mutate(ctx, mutation)

	require.Error(t, err)
	require.Contains(t, err.Error(), "PermissionDenied")
}

func TestGuardianAccess(t *testing.T) {
	ctx, _ := context.WithTimeout(context.Background(), 100*time.Second)

	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)

	testutil.DropAll(t, dg)
	op := api.Operation{Schema: "unauthpred: string @index(exact) ."}
	require.NoError(t, dg.Alter(ctx, &op))

	addNewUserToGroup(t, "guardian", "guardianpass", "guardians")

	mutation := &api.Mutation{
		SetNquads: []byte("_:a <unauthpred> \"testdata\" ."),
		CommitNow: true,
	}
	resp, err := dg.NewTxn().Mutate(ctx, mutation)
	require.NoError(t, err)

	nodeUID, ok := resp.Uids["a"]
	require.True(t, ok)

	time.Sleep(6 * time.Second)
	gClient, err := testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err, "Error while creating client")

	gClient.Login(ctx, "guardian", "guardianpass")

	mutString := fmt.Sprintf("<%s> <unauthpred> \"testdata\" .", nodeUID)
	mutation = &api.Mutation{SetNquads: []byte(mutString), CommitNow: true}
	_, err = gClient.NewTxn().Mutate(ctx, mutation)
	require.NoError(t, err, "Error while mutating unauthorized predicate")

	query := `
	{
		me(func: eq(unauthpred, "testdata")) {
			uid
		}
	}`

	resp, err = gClient.NewTxn().Query(ctx, query)
	require.NoError(t, err, "Error while querying unauthorized predicate")
	require.Contains(t, string(resp.GetJson()), "uid")

	op = api.Operation{Schema: "unauthpred: int ."}
	require.NoError(t, gClient.Alter(ctx, &op), "Error while altering unauthorized predicate")

	b := removeUserFromGroup(t, "guardian", "guardians")
	expectedOutput := `{"data":{"updateUser":{"user":[{"name":"guardian","groups":[]}]}}}`
	require.JSONEq(t, expectedOutput, string(b))

	_, err = gClient.NewTxn().Query(ctx, query)
	require.Error(t, err, "Query succeeded. It should have failed.")
}

func addNewUserToGroup(t *testing.T, userName, password, groupName string) {
	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")

	resp := createUser(t, accessJwt, userName, password)
	checkUserCount(t, resp, 1)

	addToGroup(t, accessJwt, userName, groupName)
}

func removeUserFromGroup(t *testing.T, userName, groupName string) []byte {
	removeUserGroups := `mutation updateUser($name: String!, $groupName: String!) {
			updateUser(input: {
				filter: {
					name: {
						eq: $name
					}
				},
				remove: {
					groups: [{ name: $groupName }]
				}
			}) {
				user {
					name
					groups {
						name
					}
				}
			}
		}`

	params := testutil.GraphQLParams{
		Query: removeUserGroups,
		Variables: map[string]interface{}{
			"name":      userName,
			"groupName": groupName,
		},
	}

	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")
	b := makeRequest(t, accessJwt, params)
	return b
}

func TestQueryRemoveUnauthorizedPred(t *testing.T) {
	ctx, _ := context.WithTimeout(context.Background(), 100*time.Second)

	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)

	testutil.DropAll(t, dg)
	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
		age 	 : int .
	`}
	require.NoError(t, dg.Alter(ctx, &op))

	resetUser(t)
	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")
	createGroup(t, accessJwt, devGroup)
	addToGroup(t, accessJwt, userid, devGroup)

	txn := dg.NewTxn()
	mutation := &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "RandomGuy" .
			_:a <age> "23" .
			_:a <nickname> "RG" .
			_:b <name> "RandomGuy2" .
			_:b <age> "25" .
			_:b <nickname> "RG2" .
		`),
		CommitNow: true,
	}
	_, err = txn.Mutate(ctx, mutation)
	require.NoError(t, err)

	// give read access of <name> to alice
	addRulesToGroup(t, accessJwt, devGroup, []rule{{"name", Read.Code}})

	userClient, err := testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err)
	time.Sleep(6 * time.Second)

	err = userClient.Login(ctx, userid, userpassword)
	require.NoError(t, err)

	tests := []struct {
		input       string
		output      string
		description string
	}{
		{
			`
			{
				me(func: has(name)) {
					name
					age
				}
			}
			`,
			`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			"alice doesn't have access to <age>",
		},
		{
			`
			{
				me(func: has(age)) {
					name
					age
				}
			}
			`,
			`{}`,
			`alice doesn't have access to <age> so "has(age)" is unauthorized`,
		},
		{
			`
			{
				me1(func: has(name), orderdesc: age) {
					name
					age
				}
				me2(func: has(name), orderasc: age) {
					name
					age
				}
			}
			`,
			`{"me1":[{"name":"RandomGuy"},{"name":"RandomGuy2"}],"me2":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			`me1, me2 will have same results, can't order by <age> since it is unauthorized`,
		},
		{
			`
			{
				me(func: has(name)) @groupby(age) {
					count(name)
				}
			}
			`,
			`{}`,
			`can't groupby <age> since <age> is unauthorized`,
		},
		{
			`
			{
				me(func: has(name)) @filter(eq(nickname, "RG")) {
					name
					age
				}
			}
			`,
			`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			`filter won't work because <nickname> is unauthorized`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			resp, err := userClient.NewTxn().Query(ctx, tc.input)
			require.Nil(t, err)
			testutil.CompareJSON(t, tc.output, string(resp.Json))
		})
	}
}

func TestNewACLPredicates(t *testing.T) {
	ctx, _ := context.WithTimeout(context.Background(), 100*time.Second)

	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)
	addDataAndRules(ctx, t, dg)

	userClient, err := testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err)
	time.Sleep(6 * time.Second)

	err = userClient.Login(ctx, userid, userpassword)
	require.NoError(t, err)

	queryTests := []struct {
		input       string
		output      string
		description string
	}{
		{
			`
			{
				me(func: has(name)) {
					name
					nickname
				}
			}
			`,
			`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			"alice doesn't have read access to <nickname>",
		},
		{
			`
			{
				me(func: has(nickname)) {
					name
					nickname
				}
			}
			`,
			`{}`,
			`alice doesn't have access to <nickname> so "has(nickname)" is unauthorized`,
		},
	}

	for _, tc := range queryTests {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			resp, err := userClient.NewTxn().Query(ctx, tc.input)
			require.Nil(t, err)
			testutil.CompareJSON(t, tc.output, string(resp.Json))
		})
	}

	mutationTests := []struct {
		input       string
		output      string
		err         error
		description string
	}{
		{
			"_:a <name> \"Animesh\" .",
			"",
			errors.New(""),
			"alice doesn't have write access on <name>.",
		},
		{
			"_:a <nickname> \"Pathak\" .",
			"",
			nil,
			"alice can mutate <nickname> predicate.",
		},
	}
	for _, tc := range mutationTests {
		t.Run(tc.description, func(t *testing.T) {
			_, err := userClient.NewTxn().Mutate(ctx, &api.Mutation{
				SetNquads: []byte(tc.input),
				CommitNow: true,
			})
			require.True(t, (err == nil) == (tc.err == nil))
		})
	}
}

func removeRuleFromGroup(t *testing.T, accessToken, group string, ruleID string) []byte {
	removeRuleFromGroup := `mutation updateGroup($name: String!, $rules: [RuleRef]) {
		updateGroup(input: {
			filter: {
				name: {
					eq: $name
				}
			},
			remove: {
				rules: $rules
			}
		}) {
			group {
				name
				rules {
					predicate
					permission
				}
			}
		}
	}`

	params := testutil.GraphQLParams{
		Query: removeRuleFromGroup,
		Variables: map[string]interface{}{
			"name":  group,
			"rules": []map[string]interface{}{{"id": ruleID}},
		},
	}
	b := makeRequest(t, accessToken, params)
	return b
}

func TestNegativePermissionDeleteRule(t *testing.T) {
	ctx, _ := context.WithTimeout(context.Background(), 100*time.Second)

	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)
	assigned := addDataAndRules(ctx, t, dg)

	userClient, err := testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err)
	time.Sleep(6 * time.Second)

	err = userClient.Login(ctx, userid, userpassword)
	require.NoError(t, err)

	queryName := "{me(func: has(name)) {name}}"
	resp, err := userClient.NewReadOnlyTxn().Query(ctx, queryName)
	require.NoError(t, err, "Error while querying data")

	testutil.CompareJSON(t, `{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
		string(resp.GetJson()))

	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")
	removeRuleFromGroup(t, accessJwt, devGroup, assigned["r1"])
	time.Sleep(6 * time.Second)

	resp, err = userClient.NewReadOnlyTxn().Query(ctx, queryName)
	require.NoError(t, err, "Error while querying data")
	testutil.CompareJSON(t, string(resp.GetJson()), `{}`)
}

func addDataAndRules(ctx context.Context, t *testing.T, dg *dgo.Dgraph) map[string]string {
	testutil.DropAll(t, dg)
	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
	`}
	require.NoError(t, dg.Alter(ctx, &op))

	resetUser(t)

	// TODO - We should be adding this data using the GraphQL API.
	devGroupMut := `
		_:g  <dgraph.xid>        "dev" .
		_:g  <dgraph.type>       "Group" .
		_:g  <dgraph.acl.rule>   _:r1 .
		_:r1 <dgraph.type> "Rule" .
		_:r1 <dgraph.rule.predicate>  "name" .
		_:r1 <dgraph.rule.permission> "4" .
		_:g  <dgraph.acl.rule>   _:r2 .
		_:r2 <dgraph.type> "Rule" .
		_:r2 <dgraph.rule.predicate>  "nickname" .
		_:r2 <dgraph.rule.permission> "2" .
	`
	resp, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		SetNquads: []byte(devGroupMut),
		CommitNow: true,
	})
	require.NoError(t, err, "Error adding group and permissions")

	idQuery := fmt.Sprintf(`
	{
		userid as var(func: eq(dgraph.xid, "%s"))
		gid as var(func: eq(dgraph.xid, "dev"))
	}`, userid)
	addAliceToDevMutation := &api.NQuad{
		Subject:   "uid(userid)",
		Predicate: "dgraph.user.group",
		ObjectId:  "uid(gid)",
	}
	_, err = dg.NewTxn().Do(ctx, &api.Request{
		CommitNow: true,
		Query:     idQuery,
		Mutations: []*api.Mutation{
			{
				Set: []*api.NQuad{addAliceToDevMutation},
			},
		},
	})
	require.NoError(t, err, "Error adding user to dev group")

	mutation := &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "RandomGuy" .
			_:a <nickname> "RG" .
			_:b <name> "RandomGuy2" .
			_:b <age> "25" .
			_:b <nickname> "RG2" .
		`),
		CommitNow: true,
	}
	_, err = dg.NewTxn().Mutate(ctx, mutation)
	require.NoError(t, err)
	return resp.GetUids()
}

func TestNonExistentGroup(t *testing.T) {
	t.Skip()
	// This test won't return an error anymore as if an update in a GraphQL mutation doesn't find
	// anything to update then it just returns an empty result.
	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)

	testutil.DropAll(t, dg)

	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   "groot",
		Passwd:   "password",
	})
	require.NoError(t, err, "login failed")
	addRulesToGroup(t, accessJwt, devGroup, []rule{{"name", Read.Code}})
}

func TestQueryUserInfo(t *testing.T) {
	ctx, _ := context.WithTimeout(context.Background(), 100*time.Second)

	dg, err := testutil.DgraphClientWithGroot(testutil.SockAddr)
	require.NoError(t, err)
	addDataAndRules(ctx, t, dg)

	accessJwt, _, err := testutil.HttpLogin(&testutil.LoginParams{
		Endpoint: adminEndpoint,
		UserID:   userid,
		Passwd:   userpassword,
	})
	require.NoError(t, err, "login failed")

	gqlQuery := `
	query {
		queryUser {
			name
			groups {
				name
				rules {
					predicate
					permission
				}
				users {
					name
				}
			}
		}
	}
	`

	params := testutil.GraphQLParams{
		Query: gqlQuery,
	}
	b := makeRequest(t, accessJwt, params)

	testutil.CompareJSON(t, `
	{
		"data": {
		  "queryUser": [
			{
			  "name": "alice",
			  "groups": [
				{
				  "name": "dev",
				  "rules": [
					{
					  "predicate": "name",
					  "permission": 4
					},
					{
					  "predicate": "nickname",
					  "permission": 2
					}
				  ],
				  "users": [
					  {
						  "name": "alice"
					  }
				  ]
				}
			  ]
			}
		  ]
		}
	}`, string(b))

	query := `
	{
		me(func: type(User)) {
			dgraph.xid
			dgraph.user.group {
				dgraph.xid
				dgraph.acl.rule {
					dgraph.rule.predicate
					dgraph.rule.permission
				}
			}
		}
	}
	`

	userClient, err := testutil.DgraphClient(testutil.SockAddr)
	require.NoError(t, err)

	err = userClient.Login(ctx, userid, userpassword)
	require.NoError(t, err)

	resp, err := userClient.NewReadOnlyTxn().Query(ctx, query)
	require.NoError(t, err, "Error while querying ACL")

	testutil.CompareJSON(t, `{"me":[]}`, string(resp.GetJson()))

	gqlQuery = `
	query {
		getGroup(name: "guardians") {
			name
			rules {
				predicate
				permission
			}
			users {
				name
			}
		}
	}
	`

	params = testutil.GraphQLParams{
		Query: gqlQuery,
	}
	b = makeRequest(t, accessJwt, params)
	testutil.CompareJSON(t, `{"data": {"getGroup": null}}`, string(b))
}
