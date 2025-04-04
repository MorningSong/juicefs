/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_wrapContext_WithValue(t *testing.T) {
	ctx := Background()
	key, value := "key", "value"
	withValue := func(ctx Context) Context {
		return ctx.WithValue(key, value)
	}
	assert.Equal(t, value, withValue(ctx).Value(key))
	assert.Nil(t, ctx.Value(key))
}
