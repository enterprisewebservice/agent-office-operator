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
  // ConsoleRemotePlugin wires React + react-dom + the dynamic-plugin-sdk
  // as shared singletons via module federation. DO NOT add externals
  // here — they override the plugin's federation config and the
  // bundle ends up referencing a non-existent `react` global, which
  // throws "react is not defined" → React #306 inside Console.
  plugins: [new ConsoleRemotePlugin()],
};

export default config;
