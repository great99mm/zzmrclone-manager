import React, { useState, useEffect } from 'react';
import { Settings2, Save, AlertTriangle, Lock, Eye, EyeOff, Key, ClipboardCheck } from 'lucide-react';
import { getRcloneConfig, setLogLevel, changePassword, getTokenInfo, updateToken } from '../services/api';
import useAuthStore from '../hooks/useAuthStore';
import toast from 'react-hot-toast';

const Settings = () => {
  const [config, setConfig] = useState('');
  const [logLevel, setLogLevelState] = useState('INFO');
  const [loading, setLoading] = useState(true);

  // Token state
  const [apiToken, setApiToken] = useState('');
  const [tokenEnabled, setTokenEnabled] = useState(false);
  const [showToken, setShowToken] = useState(false);

  // Password change state
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [showCurrentPassword, setShowCurrentPassword] = useState(false);
  const [showNewPassword, setShowNewPassword] = useState(false);
  const [changingPassword, setChangingPassword] = useState(false);

  const { user } = useAuthStore();

  useEffect(() => {
    loadData();
  }, []);

  const loadData = async () => {
    try {
      const [configRes, tokenRes] = await Promise.all([
        getRcloneConfig(),
        getTokenInfo(),
      ]);
      setConfig(configRes.data.content);
      // Token
      if (tokenRes.data) {
        setTokenEnabled(tokenRes.data.enabled);
        setApiToken(tokenRes.data.token || '');
      }
      // Load saved log level from localStorage
      const savedLevel = localStorage.getItem('logLevel') || 'INFO';
      setLogLevelState(savedLevel);
    } catch (err) {
      console.error('Failed to load settings');
    } finally {
      setLoading(false);
    }
  };

  const handleLogLevelChange = async (level) => {
    try {
      await setLogLevel(level);
      setLogLevelState(level);
      localStorage.setItem('logLevel', level);
      toast.success(`日志级别已切换为 ${level}`);
    } catch (err) {
      toast.error('切换失败');
    }
  };

  const handleChangePassword = async (e) => {
    e.preventDefault();

    if (newPassword !== confirmPassword) {
      toast.error('两次输入的新密码不一致');
      return;
    }

    if (newPassword.length < 6) {
      toast.error('新密码长度至少6位');
      return;
    }

    setChangingPassword(true);
    try {
      await changePassword({
        current_password: currentPassword,
        new_password: newPassword,
      });
      toast.success('密码修改成功');
      setCurrentPassword('');
      setNewPassword('');
      setConfirmPassword('');
    } catch (err) {
      toast.error(err.response?.data?.error || '密码修改失败');
    } finally {
      setChangingPassword(false);
    }
  };

  const handleSaveToken = async () => {
    try {
      await updateToken(apiToken);
      localStorage.setItem('apiToken', apiToken);
      setTokenEnabled(apiToken !== '');
      toast.success('API Token 已保存');
    } catch (err) {
      toast.error('保存失败');
    }
  };

  const handleCopyTokenUrl = () => {
    const base = window.location.origin;
    const token = localStorage.getItem('apiToken') || apiToken;
    const url = `${base}/api/output-logs?token=${token}`;
    navigator.clipboard.writeText(url).then(() => {
      toast.success('URL 已复制到剪贴板');
    }).catch(() => {
      toast.error('复制失败');
    });
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-blue-600"></div>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div>
        <h1 className="text-2xl font-bold text-gray-900">系统设置</h1>
        <p className="text-gray-500 mt-1">管理 Rclone 全局配置和系统参数</p>
      </div>

      {/* User Info */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
        <h2 className="text-lg font-semibold text-gray-900 mb-4">当前用户</h2>
        <div className="flex items-center gap-3">
          <div className="w-10 h-10 bg-blue-100 rounded-full flex items-center justify-center">
            <span className="text-blue-600 font-semibold">{user?.username?.[0]?.toUpperCase() || 'A'}</span>
          </div>
          <div>
            <div className="font-medium text-gray-900">{user?.username || 'admin'}</div>
            <div className="text-sm text-gray-500">{user?.is_admin ? '管理员' : '普通用户'}</div>
          </div>
        </div>
      </div>

      {/* Change Password */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
        <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
          <Lock className="w-5 h-5 text-blue-500" />
          修改密码
        </h2>

        <form onSubmit={handleChangePassword} className="space-y-4 max-w-md">
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">当前密码</label>
            <div className="relative">
              <input
                type={showCurrentPassword ? 'text' : 'password'}
                value={currentPassword}
                onChange={(e) => setCurrentPassword(e.target.value)}
                required
                autoComplete="current-password"
                className="w-full px-3 py-2 pr-10 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 focus:outline-none appearance-none"
                style={{ WebkitAppearance: 'none' }}
              />
              <button
                type="button"
                onClick={() => setShowCurrentPassword(!showCurrentPassword)}
                className="absolute right-3 top-1/2 -translate-y-1/2 text-gray-400 hover:text-gray-600 p-1"
                tabIndex="-1"
              >
                {showCurrentPassword ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
              </button>
            </div>
          </div>

          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">新密码</label>
            <div className="relative">
              <input
                type={showNewPassword ? 'text' : 'password'}
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                required
                minLength={6}
                autoComplete="new-password"
                className="w-full px-3 py-2 pr-10 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 focus:outline-none appearance-none"
                style={{ WebkitAppearance: 'none' }}
              />
              <button
                type="button"
                onClick={() => setShowNewPassword(!showNewPassword)}
                className="absolute right-3 top-1/2 -translate-y-1/2 text-gray-400 hover:text-gray-600 p-1"
                tabIndex="-1"
              >
                {showNewPassword ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
              </button>
            </div>
          </div>

          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">确认新密码</label>
            <input
              type="password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              required
              autoComplete="new-password"
              className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 focus:outline-none appearance-none"
              style={{ WebkitAppearance: 'none' }}
            />
          </div>

          <button
            type="submit"
            disabled={changingPassword}
            className="inline-flex items-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 transition-colors font-medium disabled:opacity-50"
          >
            <Save className="w-4 h-4" />
            {changingPassword ? '保存中...' : '修改密码'}
          </button>
        </form>
      </div>

      {/* API Token */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
        <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
          <Key className="w-5 h-5 text-blue-500" />
          API Token 设置
        </h2>
        <div className="space-y-4 max-w-md">
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">
              访问令牌 {tokenEnabled && <span className="text-green-600 text-xs">(已启用)</span>}
            </label>
            <div className="relative">
              <input
                type={showToken ? 'text' : 'password'}
                value={apiToken}
                onChange={(e) => setApiToken(e.target.value)}
                placeholder="留空表示不启用 Token 验证"
                className="w-full px-3 py-2 pr-10 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-blue-500 focus:outline-none"
              />
              <button
                type="button"
                onClick={() => setShowToken(!showToken)}
                className="absolute right-3 top-1/2 -translate-y-1/2 text-gray-400 hover:text-gray-600 p-1"
              >
                {showToken ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
              </button>
            </div>
            <p className="text-sm text-gray-500 mt-1">
              设置后，外部访问输出日志 API 需要在 URL 中添加 ?token=xxx
            </p>
          </div>
          <div className="flex items-center gap-3">
            <button
              onClick={handleSaveToken}
              className="inline-flex items-center gap-2 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 transition-colors font-medium"
            >
              <Save className="w-4 h-4" />
              保存 Token
            </button>
            {apiToken && (
              <button
                onClick={handleCopyTokenUrl}
                className="inline-flex items-center gap-2 px-4 py-2 bg-gray-100 text-gray-700 rounded-lg hover:bg-gray-200 transition-colors font-medium"
              >
                <ClipboardCheck className="w-4 h-4" />
                复制 API URL
              </button>
            )}
          </div>
        </div>
      </div>

      {/* Log Level */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
        <h2 className="text-lg font-semibold text-gray-900 mb-4">日志级别</h2>
        <div className="flex gap-2">
          {['DEBUG', 'INFO', 'NOTICE', 'ERROR'].map(level => (
            <button
              key={level}
              onClick={() => handleLogLevelChange(level)}
              className={`px-4 py-2 rounded-lg font-medium transition-colors ${
                logLevel === level 
                  ? 'bg-blue-600 text-white' 
                  : 'bg-gray-100 text-gray-700 hover:bg-gray-200'
              }`}
            >
              {level}
            </button>
          ))}
        </div>
        <p className="text-sm text-gray-500 mt-3">
          DEBUG: 最详细，包含所有调试信息 | INFO: 常规信息 | NOTICE: 重要通知 | ERROR: 仅错误
        </p>
      </div>

      {/* Rclone Config View */}
      <div className="bg-white rounded-xl shadow-sm border border-gray-200 p-6">
        <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center gap-2">
          <Settings2 className="w-5 h-5" />
          Rclone 配置预览
        </h2>
        <div className="relative">
          <pre className="bg-gray-900 text-gray-300 p-4 rounded-lg overflow-auto max-h-96 text-sm font-mono">
            {config || '无法读取配置文件'}
          </pre>
          <div className="absolute top-2 right-2">
            <span className="px-2 py-1 bg-gray-800 text-gray-400 text-xs rounded">
              只读
            </span>
          </div>
        </div>
        <p className="text-sm text-gray-500 mt-3 flex items-center gap-1">
          <AlertTriangle className="w-4 h-4 text-yellow-500" />
          配置文件通过 Docker volume 挂载，如需修改请直接编辑宿主机上的 rclone.conf
        </p>
      </div>
    </div>
  );
};

export default Settings;
