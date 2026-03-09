// Chapterhouse Admin Portal - API Client

const API_BASE = window.CH_API_BASE || '';
const MOCK_MODE = window.CH_MOCK_MODE ?? false;

let currentUser = null;

// Core API client
const api = {
    async request(method, path, body = null) {
        const options = {
            method,
            credentials: 'include',
            headers: { 'Content-Type': 'application/json' },
        };
        if (body) {
            options.body = JSON.stringify(body);
        }
        const response = await fetch(`${API_BASE}${path}`, options);

        // Handle 204 No Content
        if (response.status === 204) return null;

        const data = await response.json();
        if (!response.ok) {
            const error = new Error(data.error || 'Request failed');
            error.status = response.status;
            throw error;
        }
        return data;
    },
    get: (path) => api.request('GET', path),
    post: (path, body) => api.request('POST', path, body),
    put: (path, body) => api.request('PUT', path, body),
    delete: (path) => api.request('DELETE', path),
};

// Auth
const auth = {
    async login(username, password) {
        if (MOCK_MODE) {
            await delay(500);
            if (username === 'admin' && password === 'admin123') {
                const user = mockData.users[0];
                localStorage.setItem('ch_mock_session', JSON.stringify({ user }));
                currentUser = user;
                return user;
            }
            throw new Error('Invalid username or password');
        }
        const result = await api.post('/api/v1/admin/login', { username, password });
        currentUser = result;
        return result;
    },

    async logout() {
        if (MOCK_MODE) {
            localStorage.removeItem('ch_mock_session');
            currentUser = null;
            return;
        }
        await api.post('/api/v1/admin/logout');
        currentUser = null;
    },

    async me() {
        if (MOCK_MODE) {
            const session = localStorage.getItem('ch_mock_session');
            if (!session) throw new Error('Not authenticated');
            const { user } = JSON.parse(session);
            currentUser = user;
            return user;
        }
        const user = await api.get('/api/v1/admin/me');
        currentUser = user;
        return user;
    },

    getCurrentUser() { return currentUser; },
    isAdmin() { return currentUser?.is_admin === true; }
};

// Users API (admin)
const users = {
    async list(params = {}) {
        if (MOCK_MODE) {
            await delay(300);
            let filtered = [...mockData.users];
            if (params.active !== false) filtered = filtered.filter(u => !u.deactivated_at);
            return { data: filtered, pagination: { offset: 0, limit: 50, total: filtered.length } };
        }
        const qs = new URLSearchParams();
        if (params.limit) qs.set('limit', params.limit);
        if (params.offset) qs.set('offset', params.offset);
        if (params.active !== undefined) qs.set('active', params.active);
        return api.get(`/api/v1/admin/users?${qs}`);
    },

    async get(id) {
        if (MOCK_MODE) {
            await delay(200);
            const user = mockData.users.find(u => u.id === id);
            if (!user) throw new Error('User not found');
            return user;
        }
        return api.get(`/api/v1/admin/users/${id}`);
    },

    async create(data) {
        if (MOCK_MODE) {
            await delay(500);
            const newUser = {
                id: `u-${Date.now()}`,
                ...data,
                created_at: new Date().toISOString(),
                modified_at: new Date().toISOString(),
                deactivated_at: null,
            };
            mockData.users.push(newUser);
            return newUser;
        }
        return api.post('/api/v1/admin/users', data);
    },

    async update(id, data) {
        if (MOCK_MODE) {
            await delay(400);
            const user = mockData.users.find(u => u.id === id);
            if (!user) throw new Error('User not found');
            Object.assign(user, data, { modified_at: new Date().toISOString() });
            return user;
        }
        return api.put(`/api/v1/admin/users/${id}`, data);
    },

    async deactivate(id) {
        if (MOCK_MODE) {
            await delay(400);
            const user = mockData.users.find(u => u.id === id);
            if (user) user.deactivated_at = new Date().toISOString();
            return;
        }
        return api.delete(`/api/v1/admin/users/${id}`);
    },

    async reactivate(id) {
        if (MOCK_MODE) {
            await delay(400);
            const user = mockData.users.find(u => u.id === id);
            if (user) user.deactivated_at = null;
            return user;
        }
        return api.post(`/api/v1/admin/users/${id}/reactivate`);
    },
};

// Admin API Keys
const adminKeys = {
    async listAll(params = {}) {
        if (MOCK_MODE) {
            await delay(300);
            return { keys: mockData.keys };
        }
        const qs = new URLSearchParams();
        if (params.limit) qs.set('limit', params.limit);
        if (params.offset) qs.set('offset', params.offset);
        return api.get(`/api/v1/admin/keys?${qs}`);
    },

    async listByUser(userId) {
        if (MOCK_MODE) {
            await delay(300);
            return { keys: mockData.keys.filter(k => k.user_id === userId) };
        }
        return api.get(`/api/v1/admin/users/${userId}/keys`);
    },

    async create(userId, data) {
        if (MOCK_MODE) {
            await delay(500);
            const key = {
                id: `k-${Date.now()}`,
                user_id: userId,
                name: data.name,
                key_prefix: `ch_k1_${randomHex(8)}`,
                created_at: new Date().toISOString(),
                last_used_at: null,
                expires_at: data.expires_in ? new Date(Date.now() + parseDuration(data.expires_in)).toISOString() : null,
                revoked_at: null,
                key: `ch_k1_${randomHex(64)}`,
            };
            mockData.keys.push(key);
            return key;
        }
        return api.post(`/api/v1/admin/users/${userId}/keys`, data);
    },

    async revoke(keyId) {
        if (MOCK_MODE) {
            await delay(400);
            const key = mockData.keys.find(k => k.id === keyId);
            if (key) key.revoked_at = new Date().toISOString();
            return;
        }
        return api.delete(`/api/v1/admin/keys/${keyId}`);
    },
};

// User self-service API keys
const userKeys = {
    async list() {
        if (MOCK_MODE) {
            await delay(300);
            const userId = currentUser?.id;
            return { keys: mockData.keys.filter(k => k.user_id === userId) };
        }
        return api.get('/api/v1/user/keys');
    },

    async create(data) {
        if (MOCK_MODE) {
            await delay(500);
            const key = {
                id: `k-${Date.now()}`,
                user_id: currentUser?.id,
                name: data.name,
                key_prefix: `ch_k1_${randomHex(8)}`,
                created_at: new Date().toISOString(),
                last_used_at: null,
                expires_at: data.expires_in ? new Date(Date.now() + parseDuration(data.expires_in)).toISOString() : null,
                revoked_at: null,
                key: `ch_k1_${randomHex(64)}`,
            };
            mockData.keys.push(key);
            return key;
        }
        return api.post('/api/v1/user/keys', data);
    },

    async revoke(keyId) {
        if (MOCK_MODE) {
            await delay(400);
            const key = mockData.keys.find(k => k.id === keyId);
            if (key) key.revoked_at = new Date().toISOString();
            return;
        }
        return api.delete(`/api/v1/user/keys/${keyId}`);
    },
};

// User self-service
const userAccount = {
    async changePassword(currentPassword, newPassword) {
        if (MOCK_MODE) {
            await delay(500);
            return;
        }
        return api.post('/api/v1/user/password', { current_password: currentPassword, new_password: newPassword });
    },
};

// Admin audit log
const adminAudit = {
    async list(params = {}) {
        if (MOCK_MODE) {
            await delay(300);
            let entries = [...mockData.auditEntries];
            if (params.action) entries = entries.filter(e => e.action === params.action);
            if (params.resource_type) entries = entries.filter(e => e.resource_type === params.resource_type);
            return { entries, total: entries.length };
        }
        const qs = new URLSearchParams();
        if (params.limit) qs.set('limit', params.limit);
        if (params.offset) qs.set('offset', params.offset);
        if (params.action) qs.set('action', params.action);
        if (params.resource_type) qs.set('resource_type', params.resource_type);
        if (params.user_id) qs.set('user_id', params.user_id);
        return api.get(`/api/v1/admin/audit?${qs}`);
    },
};

// User audit log
const userAudit = {
    async list(params = {}) {
        if (MOCK_MODE) {
            await delay(300);
            let entries = mockData.auditEntries.filter(e => e.actor_id === currentUser?.id);
            return { entries, total: entries.length };
        }
        const qs = new URLSearchParams();
        if (params.limit) qs.set('limit', params.limit);
        if (params.offset) qs.set('offset', params.offset);
        if (params.action) qs.set('action', params.action);
        if (params.resource_type) qs.set('resource_type', params.resource_type);
        return api.get(`/api/v1/user/audit?${qs}`);
    },
};

// Stats
const stats = {
    async get() {
        if (MOCK_MODE) {
            await delay(200);
            return mockData.stats;
        }
        return api.get('/api/v1/admin/stats');
    },

    async getSystem() {
        if (MOCK_MODE) {
            await delay(200);
            return mockData.systemStats;
        }
        return api.get('/api/v1/admin/system-stats');
    },

    async getMemoryTypeDistribution() {
        if (MOCK_MODE) {
            await delay(200);
            return mockData.memoryTypeDistribution;
        }
        return api.get('/api/v1/admin/memory-type-distribution');
    },

    async getMemoryScopeDistribution() {
        if (MOCK_MODE) {
            await delay(200);
            return mockData.memoryScopeDistribution;
        }
        return api.get('/api/v1/admin/memory-scope-distribution');
    },

    async getTopTags(limit = 20) {
        if (MOCK_MODE) {
            await delay(200);
            return mockData.topTags;
        }
        return api.get(`/api/v1/admin/top-tags?limit=${limit}`);
    },
};

// Utility functions
function delay(ms) {
    return new Promise(resolve => setTimeout(resolve, ms));
}

function randomHex(length) {
    return Array.from({ length }, () => Math.floor(Math.random() * 16).toString(16)).join('');
}

function parseDuration(str) {
    const match = str.match(/^(\d+)(d|w|m|y)$/);
    if (!match) return parseInt(str) * 86400000; // default to days
    const [, num, unit] = match;
    const ms = { d: 86400000, w: 604800000, m: 2592000000, y: 31536000000 };
    return parseInt(num) * ms[unit];
}

function formatDate(dateStr) {
    if (!dateStr) return '-';
    const date = new Date(dateStr);
    return date.toLocaleDateString('en-US', {
        year: 'numeric', month: 'short', day: 'numeric',
        hour: '2-digit', minute: '2-digit'
    });
}

function formatRelativeTime(dateStr) {
    if (!dateStr) return '-';
    const diff = Date.now() - new Date(dateStr).getTime();
    const seconds = Math.floor(diff / 1000);
    const minutes = Math.floor(seconds / 60);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (seconds < 60) return 'Just now';
    if (minutes < 60) return `${minutes}m ago`;
    if (hours < 24) return `${hours}h ago`;
    if (days < 7) return `${days}d ago`;
    return formatDate(dateStr);
}

function getInitials(name) {
    if (!name) return '??';
    return name.split(/[\s_-]/).map(n => n[0]).join('').toUpperCase().slice(0, 2);
}

function formatBytes(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(1024));
    return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

function truncateId(id) {
    if (!id) return '-';
    return id.length > 12 ? id.slice(0, 8) + '...' : id;
}

function keyStatus(key) {
    if (key.revoked_at) return 'revoked';
    if (key.expires_at && new Date(key.expires_at) < new Date()) return 'expired';
    return 'active';
}

// Mock data
const mockData = {
    users: [
        {
            id: 'a1b2c3d4-0000-0000-0000-000000000001',
            username: 'admin',
            email: 'admin@example.com',
            display_name: 'Admin User',
            is_admin: true,
            created_at: '2025-01-15T10:00:00Z',
            modified_at: '2025-01-15T10:00:00Z',
            deactivated_at: null,
        },
        {
            id: 'a1b2c3d4-0000-0000-0000-000000000002',
            username: 'alice',
            email: 'alice@example.com',
            display_name: 'Alice Smith',
            is_admin: false,
            created_at: '2025-02-01T14:30:00Z',
            modified_at: '2025-02-01T14:30:00Z',
            deactivated_at: null,
        },
        {
            id: 'a1b2c3d4-0000-0000-0000-000000000003',
            username: 'bob',
            email: 'bob@example.com',
            display_name: 'Bob Johnson',
            is_admin: false,
            created_at: '2025-02-10T11:00:00Z',
            modified_at: '2025-02-10T11:00:00Z',
            deactivated_at: null,
        },
    ],

    keys: [
        {
            id: 'k-001',
            user_id: 'a1b2c3d4-0000-0000-0000-000000000001',
            username: 'admin',
            name: 'Production Key',
            key_prefix: 'ch_k1_7f3a8b2c',
            created_at: '2025-01-15T10:30:00Z',
            last_used_at: new Date().toISOString(),
            expires_at: null,
            revoked_at: null,
        },
        {
            id: 'k-002',
            user_id: 'a1b2c3d4-0000-0000-0000-000000000002',
            username: 'alice',
            name: 'Development',
            key_prefix: 'ch_k1_2b8c9d1e',
            created_at: '2025-02-01T14:45:00Z',
            last_used_at: '2025-02-03T08:00:00Z',
            expires_at: '2025-05-01T14:45:00Z',
            revoked_at: null,
        },
        {
            id: 'k-003',
            user_id: 'a1b2c3d4-0000-0000-0000-000000000003',
            username: 'bob',
            name: 'Testing',
            key_prefix: 'ch_k1_9d1e4f5a',
            created_at: '2025-02-10T11:30:00Z',
            last_used_at: null,
            expires_at: '2025-04-10T11:30:00Z',
            revoked_at: null,
        },
    ],

    auditEntries: [
        {
            id: 1,
            actor_id: 'a1b2c3d4-0000-0000-0000-000000000001',
            actor_username: 'admin',
            action: 'login',
            resource_type: 'session',
            resource_id: null,
            details: null,
            ip_address: '192.168.1.100',
            created_at: new Date(Date.now() - 2 * 60 * 1000).toISOString(),
        },
        {
            id: 2,
            actor_id: 'a1b2c3d4-0000-0000-0000-000000000001',
            actor_username: 'admin',
            action: 'create',
            resource_type: 'api_key',
            resource_id: 'k-002',
            target_username: 'alice',
            details: { key_name: 'Development' },
            ip_address: '192.168.1.100',
            created_at: new Date(Date.now() - 15 * 60 * 1000).toISOString(),
        },
        {
            id: 3,
            actor_id: 'a1b2c3d4-0000-0000-0000-000000000001',
            actor_username: 'admin',
            action: 'create',
            resource_type: 'user',
            resource_id: 'a1b2c3d4-0000-0000-0000-000000000002',
            target_username: 'alice',
            details: null,
            ip_address: '192.168.1.100',
            created_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
        },
        {
            id: 4,
            actor_id: 'a1b2c3d4-0000-0000-0000-000000000002',
            actor_username: 'alice',
            action: 'mcp.remember',
            resource_type: 'memory',
            resource_id: null,
            details: { fact: 'K3s is the preferred Kubernetes runtime' },
            ip_address: '10.0.0.50',
            created_at: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
        },
        {
            id: 5,
            actor_id: 'a1b2c3d4-0000-0000-0000-000000000002',
            actor_username: 'alice',
            action: 'mcp.recall',
            resource_type: 'memory',
            resource_id: null,
            details: { query: 'what database does the project use?' },
            ip_address: '10.0.0.50',
            created_at: new Date(Date.now() - 4 * 60 * 60 * 1000).toISOString(),
        },
    ],

    stats: {
        active_users: 24,
        admin_users: 3,
        active_api_keys: 42,
        active_sessions: 8,
    },

    systemStats: {
        postgres: {
            total_conns: 20,
            acquired_conns: 5,
            idle_conns: 15,
            max_conns: 20,
            acquire_count: 1024,
            empty_acquire_count: 12,
            canceled_acquire_count: 0,
        },
        memory: {
            users_with_memories: 18,
            total_memory_blocks: 342,
            total_content_bytes: 524288,
            unique_memory_names: 128,
        },
    },

    memoryTypeDistribution: [
        { memory_type: 'core', count: 85 },
        { memory_type: 'index', count: 157 },
        { memory_type: 'state', count: 100 },
    ],

    memoryScopeDistribution: [
        { scope: 'global', count: 45 },
        { scope: 'user', count: 297 },
    ],

    topTags: [
        { tag: 'kubernetes', count: 34 },
        { tag: 'architecture', count: 28 },
        { tag: 'debugging', count: 22 },
        { tag: 'infrastructure', count: 19 },
        { tag: 'api-design', count: 15 },
        { tag: 'testing', count: 12 },
        { tag: 'security', count: 10 },
        { tag: 'performance', count: 8 },
    ],
};

// Export for global access
window.chapterhouse = {
    api,
    auth,
    users,
    adminKeys,
    userKeys,
    userAccount,
    adminAudit,
    userAudit,
    stats,
    formatDate,
    formatRelativeTime,
    getInitials,
    formatBytes,
    truncateId,
    keyStatus,
    MOCK_MODE,
};
