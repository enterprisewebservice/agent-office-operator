import { Configuration } from 'webpack';
import { ConsoleRemotePlugin } from '@openshift-console/dynamic-plugin-sdk-webpack';

const config: Configuration = {
  mode: 'production',
  entry: {},
  context: process.cwd(),
  output: {
    path: process.cwd() + '/dist',
    filename: '[name]-bundle.js',
    chunkFilename: '[name]-chunk.js',
  },
  resolve: {
    extensions: ['.ts', '.tsx', '.js', '.jsx'],
  },
  module: {
    rules: [
      {
        test: /\.(ts|tsx)$/,
        exclude: /node_modules/,
        use: [{ loader: 'ts-loader' }],
      },
      {
        test: /\.css$/,
        use: ['style-loader', 'css-loader'],
      },
    ],
  },
  plugins: [new ConsoleRemotePlugin()],
  externals: {
    react: 'react',
    'react-dom': 'react-dom',
  },
};

export default config;
